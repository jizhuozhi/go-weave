//go:build darwin && arm64

package weave

import (
	"runtime"
	"syscall"
	"unsafe"
)

// jitExecAlloc allocates an executable region, writes code into it and makes it
// executable. On Apple Silicon a MAP_JIT mapping is never writable and
// executable at once: it starts RX, we disable write-protection to write, and
// re-enable it before returning. pthread_jit_write_protect_np is per-thread, so
// the toggle-and-write sequence is pinned to one OS thread with LockOSThread —
// otherwise the goroutine could migrate between the two toggles and leave a
// thread in RW state, faulting the next time it executes any MAP_JIT page.
func jitExecAlloc(code []byte) ([]byte, error) {
	mem, err := syscall.Mmap(-1, 0, len(code),
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_ANON|syscall.MAP_PRIVATE|0x800) // 0x800 = MAP_JIT
	if err != nil {
		return nil, err
	}

	runtime.LockOSThread()
	jitWriteProtect(false) // RX -> RW
	copy(mem, code)
	jitWriteProtect(true) // RW -> RX
	jitFlushICache(uintptr(unsafe.Pointer(&mem[0])), uintptr(len(mem)))
	runtime.UnlockOSThread()

	return mem, nil
}
