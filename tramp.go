package weave

// Trampoline selection.
//
// itab.Fun[k] must hold a bare code pointer: the compiler's interface dispatch
// is `MOVD 24(itab), R6; CALL (R6)` and passes no closure context. A trampoline
// therefore cannot be a closure and cannot be a reflect.MakeFunc value (whose
// entry expects its context in the closure register). Instead, method k of an
// interface gets generated function k (see gen and stubs_gen_*.go), whose code
// pointer carries no context but whose hardcoded index is the method's index in
// the interface. The dispatcher resolves the *Method — and with it the proxy's
// delegation target — through the receiver, so the same slots serve every
// interface; only the methods-per-interface count is bounded.

import (
	"fmt"
	"unsafe"
)

// newTrampoline returns the code pointer for method index m.Index.
func newTrampoline(m *Method) unsafe.Pointer {
	if !fastTrampoline {
		panic("weave: dynamic proxies are only supported on amd64 and arm64")
	}
	l := m.layout
	if len(l.stackPtrOffs) > 0 {
		panic("weave: method " + m.Name + " " + m.Type.String() +
			" passes pointers in its stack-assigned arguments, which the trampoline cannot keep visible to the collector;" +
			" restructure the signature so pointer arguments stay within the register file")
	}
	if l.stackBytes > stackWindow {
		panic(fmt.Sprintf("weave: method %s needs %d bytes of stack argument area, more than the trampoline window of %d;"+
			" raise stackWindow in gen and the redial frame, and run go generate",
			m.Name+" "+m.Type.String(), l.stackBytes, stackWindow))
	}
	return fastStub(m.Index)
}
