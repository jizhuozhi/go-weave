package weave

// Trampoline selection.
//
// itab.Fun[k] must hold a bare code pointer: the compiler's interface dispatch
// is `MOVD 24(itab), R6; CALL (R6)` and passes no closure context. A trampoline
// therefore cannot be a closure and cannot be a reflect.MakeFunc value (whose
// entry expects its context in the closure register). Instead, method k of an
// interface gets runtime-generated trampoline k (see jit.go), whose code pointer
// carries no context but whose hardcoded index is the method's index in the
// interface. The dispatcher resolves the *Method — and with it the proxy's
// delegation target — through the receiver, so the same slots serve every
// interface; only the methods-per-interface count is bounded.
//
// Methods that move pointers through the caller's stack argument area need a
// trampoline that describes that area precisely, which the generic one cannot;
// see precise.go.

import (
	"fmt"
	"unsafe"
)

// newTrampoline returns the code pointer for method index m.Index.
func newTrampoline(m *Method) unsafe.Pointer {
	if !hasTrampoline {
		panic("weave: dynamic proxies are only supported on amd64 and arm64")
	}
	l := m.layout
	if l.stackBytes > stackWindow {
		panic(fmt.Sprintf("weave: method %s needs %d bytes of stack argument area, more than the trampoline window of %d;"+
			" raise stackWindow and the redial frame",
			m.Name+" "+m.Type.String(), l.stackBytes, stackWindow))
	}
	if !l.stackPointers() {
		// Nothing but plain data crosses the stack argument area, which is
		// exactly what the generic trampoline's byte window describes.
		return slotCode(m.Index)
	}
	sh := l.shape(m.Index)
	// The trampoline is compiled at proxy construction — a one-off, cached cost
	// per shape — and the call site stays a plain indirect itab call.
	if code := jitTrampoline(sh); code != nil {
		return code
	}
	panic("weave: " + m.Name + " " + m.Type.String() +
		" moves pointers through the caller's stack argument area (" + sh.String() + ")," +
		" and no trampoline could be generated." +
		"\nKeep pointer arguments and results inside the register file: register assignment is positional," +
		" so moving pointer arguments to the front of the signature is often enough (the receiver takes one" +
		" word, " + fmt.Sprint(intArgRegs-1) + " integer words remain).")
}
