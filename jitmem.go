//go:build !darwin || !arm64

package weave

import "syscall"

// jitExecAlloc allocates a writable, executable region for one trampoline. On
// platforms without MAP_JIT write-protection — Linux, the BSDs, and Intel
// macOS — a plain RWX mapping suffices.
func jitExecAlloc(n int) (mem []byte, makeExec func(), err error) {
	mem, err = syscall.Mmap(-1, 0, n,
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil, nil, err
	}
	return mem, func() {}, nil
}
