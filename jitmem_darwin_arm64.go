//go:build darwin && arm64

package weave

import (
	"syscall"
	"unsafe"
)

// jitExecAlloc allocates a writable, executable region on Apple Silicon, where
// a MAP_JIT mapping is never writable and executable at once: it starts RX, we
// disable write-protection to write, and makeExec re-enables it and invalidates
// the instruction cache.
func jitExecAlloc(n int) (mem []byte, makeExec func(), err error) {
	mem, err = syscall.Mmap(-1, 0, n,
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_ANON|syscall.MAP_PRIVATE|0x800) // 0x800 = MAP_JIT
	if err != nil {
		return nil, nil, err
	}
	jitWriteProtect(false) // disable write-protect: RX -> RW
	makeExec = func() {
		jitWriteProtect(true) // enable write-protect: RW -> RX
		jitFlushICache(uintptr(unsafe.Pointer(&mem[0])), uintptr(len(mem)))
	}
	return mem, makeExec, nil
}
