//go:build !arm64 && !amd64

package weave

import "unsafe"

// No trampoline for this architecture.
const hasTrampoline = false

func slotCode(i int) unsafe.Pointer {
	panic("weave: no trampoline on this architecture")
}

func redial(fun unsafe.Pointer, regs *regBuf) {
	panic("weave: no trampoline on this architecture")
}
