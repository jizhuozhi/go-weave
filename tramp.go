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

import "unsafe"

// newTrampoline returns the code pointer for method index m.Index.
func newTrampoline(m *Method) unsafe.Pointer {
	if !fastTrampoline {
		panic("weave: dynamic proxies are only supported on amd64 and arm64")
	}
	if !m.fitsFastPath() {
		panic("weave: method " + m.Name + " " + m.Type.String() +
			" spills arguments or results to the stack, which the fixed trampoline cannot express")
	}
	return fastStub(m.Index)
}
