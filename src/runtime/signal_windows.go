// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"runtime/internal/sys"
	"unsafe"
)

func disableWER() {
	// do not display Windows Error Reporting dialogue
	const (
		SEM_FAILCRITICALERRORS     = 0x0001
		SEM_NOGPFAULTERRORBOX      = 0x0002
		SEM_NOALIGNMENTFAULTEXCEPT = 0x0004
		SEM_NOOPENFILEERRORBOX     = 0x8000
	)
	errormode := uint32(stdcall1(_SetErrorMode, SEM_NOGPFAULTERRORBOX))
	stdcall1(_SetErrorMode, uintptr(errormode)|SEM_FAILCRITICALERRORS|SEM_NOGPFAULTERRORBOX|SEM_NOOPENFILEERRORBOX)
}

// isWin7 returns true on Windows 7. Otherwise it returns false.
//
//go:nosplit
func isWin7() bool {
	var maj, min, build uint32
	stdcall3(_RtlGetNtVersionNumbers, uintptr(unsafe.Pointer(&maj)), uintptr(unsafe.Pointer(&min)), uintptr(unsafe.Pointer(&build)))
	return maj < 6 || (maj == 6 && min <= 1)
}

// enableWERNoUI re-enables Windows error reporting without fault reporting UI.
//
// This is marked nosplit since it is used during crash.
//
//go:nosplit
func enableWERNoUI() bool {
	if _WerSetFlags == nil {
		return false
	}

	// Disable Fault reporting UI
	const (
		WER_FAULT_REPORTING_NO_UI = 0x0020
	)
	if stdcall1(_WerSetFlags, WER_FAULT_REPORTING_NO_UI) != 0 {
		return false
	}

	// re-enable Windows Error Reporting
	stdcall1(_SetErrorMode, 0)
	return true
}

// in sys_windows_386.s and sys_windows_amd64.s
func exceptiontramp()
func firstcontinuetramp()
func lastcontinuetramp()

func initExceptionHandler() {
	stdcall2(_AddVectoredExceptionHandler, 1, abi.FuncPCABI0(exceptiontramp))
	if _AddVectoredContinueHandler == nil || GOARCH == "386" {
		// use SetUnhandledExceptionFilter for windows-386 or
		// if VectoredContinueHandler is unavailable.
		// note: SetUnhandledExceptionFilter handler won't be called, if debugging.
		stdcall1(_SetUnhandledExceptionFilter, abi.FuncPCABI0(lastcontinuetramp))
	} else {
		stdcall2(_AddVectoredContinueHandler, 1, abi.FuncPCABI0(firstcontinuetramp))
		stdcall2(_AddVectoredContinueHandler, 0, abi.FuncPCABI0(lastcontinuetramp))
	}
}

// isAbort returns true, if context r describes exception raised
// by calling runtime.abort function.
//
//go:nosplit
func isAbort(r *context) bool {
	pc := r.ip()
	if GOARCH == "386" || GOARCH == "amd64" || GOARCH == "arm" {
		// In the case of an abort, the exception IP is one byte after
		// the INT3 (this differs from UNIX OSes). Note that on ARM,
		// this means that the exception IP is no longer aligned.
		pc--
	}
	return isAbortPC(pc)
}

// isgoexception reports whether this exception should be translated
// into a Go panic or throw.
//
// It is nosplit to avoid growing the stack in case we're aborting
// because of a stack overflow.
//
//go:nosplit
func isgoexception(info *exceptionrecord, r *context) bool {
	// Only handle exception if executing instructions in Go binary
	// (not Windows library code).
	// TODO(mwhudson): needs to loop to support shared libs
	if r.ip() < firstmoduledata.text || firstmoduledata.etext < r.ip() {
		return false
	}

	// Go will only handle some exceptions.
	switch info.exceptioncode {
	default:
		return false
	case _EXCEPTION_ACCESS_VIOLATION:
	case _EXCEPTION_INT_DIVIDE_BY_ZERO:
	case _EXCEPTION_INT_OVERFLOW:
	case _EXCEPTION_FLT_DENORMAL_OPERAND:
	case _EXCEPTION_FLT_DIVIDE_BY_ZERO:
	case _EXCEPTION_FLT_INEXACT_RESULT:
	case _EXCEPTION_FLT_OVERFLOW:
	case _EXCEPTION_FLT_UNDERFLOW:
	case _EXCEPTION_BREAKPOINT:
	case _EXCEPTION_ILLEGAL_INSTRUCTION: // breakpoint arrives this way on arm64
	}
	return true
}

// Called by sigtramp from Windows VEH handler.
// Return value signals whether the exception has been handled (EXCEPTION_CONTINUE_EXECUTION)
// or should be made available to other handlers in the chain (EXCEPTION_CONTINUE_SEARCH).
//
// This is the first entry into Go code for exception handling. This
// is nosplit to avoid growing the stack until we've checked for
// _EXCEPTION_BREAKPOINT, which is raised if we overflow the g0 stack,
//
//go:nosplit
func exceptionhandler(info *exceptionrecord, r *context, gp *g) int32 {
	if !isgoexception(info, r) {
		return _EXCEPTION_CONTINUE_SEARCH
	}

	if gp.throwsplit || isAbort(r) {
		// We can't safely sigpanic because it may grow the stack.
		// Or this is a call to abort.
		// Don't go through any more of the Windows handler chain.
		// Crash now.
		winthrow(info, r, gp)
		exit(2)
	}

	// After this point, it is safe to grow the stack.

	// Make it look like a call to the signal func.
	// Have to pass arguments out of band since
	// augmenting the stack frame would break
	// the unwinding code.
	gp.sig = info.exceptioncode
	gp.sigcode0 = info.exceptioninformation[0]
	gp.sigcode1 = info.exceptioninformation[1]
	gp.sigpc = r.ip()

	// Only push runtime·sigpanic if r.ip() != 0.
	// If r.ip() == 0, probably panicked because of a
	// call to a nil func. Not pushing that onto sp will
	// make the trace look like a call to runtime·sigpanic instead.
	// (Otherwise the trace will end at runtime·sigpanic and we
	// won't get to see who faulted.)
	// Also don't push a sigpanic frame if the faulting PC
	// is the entry of asyncPreempt. In this case, we suspended
	// the thread right between the fault and the exception handler
	// starting to run, and we have pushed an asyncPreempt call.
	// The exception is not from asyncPreempt, so not to push a
	// sigpanic call to make it look like that. Instead, just
	// overwrite the PC. (See issue #35773)
	if r.ip() != 0 && r.ip() != abi.FuncPCABI0(asyncPreempt) {
		sp := unsafe.Pointer(r.sp())
		delta := uintptr(sys.StackAlign)
		sp = add(sp, -delta)
		r.set_sp(uintptr(sp))
		if usesLR {
			*((*uintptr)(sp)) = r.lr()
			r.set_lr(r.ip())
		} else {
			*((*uintptr)(sp)) = r.ip()
		}
	}
	r.set_ip(abi.FuncPCABI0(sigpanic0))
	return _EXCEPTION_CONTINUE_EXECUTION
}

// It seems Windows searches ContinueHandler's list even
// if ExceptionHandler returns EXCEPTION_CONTINUE_EXECUTION.
// firstcontinuehandler will stop that search,
// if exceptionhandler did the same earlier.
//
// It is nosplit for the same reason as exceptionhandler.
//
//go:nosplit
func firstcontinuehandler(info *exceptionrecord, r *context, gp *g) int32 {
	if !isgoexception(info, r) {
		return _EXCEPTION_CONTINUE_SEARCH
	}
	return _EXCEPTION_CONTINUE_EXECUTION
}

var testingWER bool

// lastcontinuehandler is reached, because runtime cannot handle
// current exception. lastcontinuehandler will print crash info and exit.
//
// It is nosplit for the same reason as exceptionhandler.
//
//go:nosplit
func lastcontinuehandler(info *exceptionrecord, r *context, gp *g) int32 {
	if islibrary || isarchive {
		// Go DLL/archive has been loaded in a non-go program.
		// If the exception does not originate from go, the go runtime
		// should not take responsibility of crashing the process.
		return _EXCEPTION_CONTINUE_SEARCH
	}
	if testingWER {
		return _EXCEPTION_CONTINUE_SEARCH
	}

	// VEH is called before SEH, but arm64 MSVC DLLs use SEH to trap
	// illegal instructions during runtime initialization to determine
	// CPU features, so if we make it to the last handler and we're
	// arm64 and it's an illegal instruction and this is coming from
	// non-Go code, then assume it's this runtime probing happen, and
	// pass that onward to SEH.
	if GOARCH == "arm64" && info.exceptioncode == _EXCEPTION_ILLEGAL_INSTRUCTION &&
		(r.ip() < firstmoduledata.text || firstmoduledata.etext < r.ip()) {
		return _EXCEPTION_CONTINUE_SEARCH
	}

	winthrow(info, r, gp)

	_, _, docrash := gotraceback()
	if docrash {
		// Windows 7 apears to ignore WER_FAULT_REPORTING_NO_UI
		// WerSetFlags API flag. So do not call enableWERNoUI
		// on Windows 7.
		if !isWin7() {
			// trigger crash dump creation
			if enableWERNoUI() {
				return _EXCEPTION_CONTINUE_SEARCH
			}
		}
	}
	exit(2)
	return 0 // not reached
}

//go:nosplit
func winthrow(info *exceptionrecord, r *context, gp *g) {
	_g_ := getg()

	// In case we're handling a g0 stack overflow, blow away the
	// g0 stack bounds so we have room to print the traceback. If
	// this somehow overflows the stack, the OS will trap it.
	_g_.stack.lo = 0
	_g_.stackguard0 = _g_.stack.lo + _StackGuard
	_g_.stackguard1 = _g_.stackguard0

	print("Exception ", hex(info.exceptioncode), " ", hex(info.exceptioninformation[0]), " ", hex(info.exceptioninformation[1]), " ", hex(r.ip()), "\n")

	print("PC=", hex(r.ip()), "\n")
	if _g_.m.incgo && gp == _g_.m.g0 && _g_.m.curg != nil {
		if iscgo {
			print("signal arrived during external code execution\n")
		}
		gp = _g_.m.curg
	}
	print("\n")

	_g_.m.throwing = 1
	_g_.m.caughtsig.set(gp)

	level, _, _ := gotraceback()
	if level > 0 {
		// only print traceback when it hasn't been printed
		if tracebackprinted == 0 {
			tracebacktrap(r.ip(), r.sp(), r.lr(), gp)
			tracebackothers(gp)
			tracebackprinted = 1
		}
		dumpregs(r)
	}
}

func sigpanic() {
	g := getg()
	if !canpanic(g) {
		throw("unexpected signal during runtime execution")
	}

	switch g.sig {
	case _EXCEPTION_ACCESS_VIOLATION:
		if g.sigcode1 < 0x1000 {
			panicmem()
		}
		if g.paniconfault {
			panicmemAddr(g.sigcode1)
		}
		print("unexpected fault address ", hex(g.sigcode1), "\n")
		throw("fault")
	case _EXCEPTION_INT_DIVIDE_BY_ZERO:
		panicdivide()
	case _EXCEPTION_INT_OVERFLOW:
		panicoverflow()
	case _EXCEPTION_FLT_DENORMAL_OPERAND,
		_EXCEPTION_FLT_DIVIDE_BY_ZERO,
		_EXCEPTION_FLT_INEXACT_RESULT,
		_EXCEPTION_FLT_OVERFLOW,
		_EXCEPTION_FLT_UNDERFLOW:
		panicfloat()
	}
	throw("fault")
}

var (
	badsignalmsg [100]byte
	badsignallen int32
)

func setBadSignalMsg() {
	const msg = "runtime: signal received on thread not created by Go.\n"
	for i, c := range msg {
		badsignalmsg[i] = byte(c)
		badsignallen++
	}
}

// Following are not implemented.

func initsig(preinit bool) {
}

func sigenable(sig uint32) {
}

func sigdisable(sig uint32) {
}

func sigignore(sig uint32) {
}

func badsignal2()

func raisebadsignal(sig uint32) {
	badsignal2()
}

func signame(sig uint32) string {
	return ""
}

//go:nosplit
func crash() {
	// When GOTRACEBACK==crash, raise the same exception
	// from kernel32.dll, so that Windows gets a chance
	// to handle the exception by creating a crash dump.

	// Get the Exception code that caused the crash
	gp := getg()
	exceptionCode := gp.sig

	// RaiseException() here will not be handled in exceptionhandler()
	// because it comes from kernel32.dll
	stdcall4(_RaiseException, uintptr(unsafe.Pointer(&exceptionCode)), 0, 0, 0)
}

// gsignalStack is unused on Windows.
type gsignalStack struct{}
