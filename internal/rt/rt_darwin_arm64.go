//go:build darwin && arm64

package rt

/*
#cgo CFLAGS: -D_DARWIN_C_SOURCE
#include <pthread.h>
#include <libkern/OSCacheControl.h>
#include <sys/mman.h>
#include <string.h>

static void weave_flush_icache(void *addr, size_t n) {
	sys_icache_invalidate(addr, n);
}

// selftest runs the canonical Apple Silicon JIT round trip entirely in C:
// mmap RWX+MAP_JIT, disable write-protect to write, re-enable to execute.
static int weave_jit_selftest(void) {
	void *p = mmap(NULL, 4096, PROT_READ|PROT_WRITE|PROT_EXEC, MAP_ANON|MAP_PRIVATE|MAP_JIT, -1, 0);
	if (p == MAP_FAILED) return 1;
	uint32_t ret = 0xd65f03c0; // RET
	pthread_jit_write_protect_np(0); // disable write-protect: RX -> RW
	memcpy(p, &ret, 4);
	pthread_jit_write_protect_np(1); // enable write-protect: RW -> RX
	sys_icache_invalidate(p, 4);
	((void (*)(void))p)(); // must not fault
	pthread_jit_write_protect_np(0);
	munmap(p, 4096);
	return 0;
}
*/
import "C"

import "unsafe"

func jitWriteProtect(on bool) {
	v := C.int(0)
	if on {
		v = 1
	}
	C.pthread_jit_write_protect_np(v)
}

// FlushICache invalidates the instruction cache for the range, which Apple
// Silicon requires after writing machine code before executing it.
func FlushICache(addr uintptr, n uintptr) {
	C.weave_flush_icache(unsafe.Pointer(addr), C.size_t(n))
}

// Selftest runs a C-level mmap+protect+execute round trip and reports whether
// it faulted, isolating Go-side issues from the platform mechanism.
func Selftest() bool { return C.weave_jit_selftest() == 0 }
