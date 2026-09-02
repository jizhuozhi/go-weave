//go:build !arm64 && !amd64

package weave

import "unsafe"

// No fast trampoline for this architecture.
const fastTrampoline = false

func fastStub(i int) unsafe.Pointer {
	panic("weave: no fast trampoline on this architecture")
}

func redial(fun unsafe.Pointer, regs *regBuf) {
	panic("weave: no fast trampoline on this architecture")
}
