//go:build !darwin || !arm64

package weave

import "syscall"

// jitExecAlloc allocates a writable, executable region and writes code into it.
// On platforms without MAP_JIT write-protection — Linux, the BSDs, and Intel
// macOS — a plain RWX mapping is enough and needs no toggle.
func jitExecAlloc(code []byte) ([]byte, error) {
	mem, err := syscall.Mmap(-1, 0, len(code),
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	copy(mem, code)
	return mem, nil
}
