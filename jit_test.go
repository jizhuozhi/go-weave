//go:build amd64 || arm64

package weave

import (
	"runtime"
	"syscall"
	"testing"
	"unsafe"

	"github.com/jizhuozhi/go-weave/internal/rt"
)

// TestJITFindfunc verifies the layout mirror is exact and a registered module
// is discoverable by runtime.FuncForPC.
func TestJITFindfunc(t *testing.T) {
	// Sanity: the mirror's field offsets must agree with the runtime's. The
	// expected values are version-specific (md*Off constants live next to the
	// jitModuledata mirror for this Go version).
	if got := unsafe.Offsetof(lastmoduledatap.text); got != mdTextOff {
		t.Fatalf("moduledata.text offset = %d, want %d", got, mdTextOff)
	}
	if got := unsafe.Offsetof(lastmoduledatap.gofunc); got != mdGofuncOff {
		t.Fatalf("moduledata.gofunc offset = %d, want %d", got, mdGofuncOff)
	}
	if got := unsafe.Offsetof(lastmoduledatap.next); got != mdNextOff {
		t.Fatalf("moduledata.next offset = %d, want %d", got, mdNextOff)
	}
	if got := unsafe.Offsetof(lastmoduledatap.pctab); got != mdPctabOff {
		t.Fatalf("moduledata.pctab offset = %d, want %d", got, mdPctabOff)
	}

	// This first step only probes findfunc, so a plain writable mapping stands
	// in for the text range — findmoduledatap matches on [minpc, maxpc) and
	// never checks executability. (Real execution needs MAP_JIT +
	// pthread_jit_write_protect_np on Apple Silicon, exercised by the tests in
	// precise_test.go.)
	code, err := syscall.Mmap(-1, 0, 4096,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Skipf("mmap: %v", err)
	}

	jitMu.Lock()
	md := buildJITModule(code[:16], 0, 0, 0, 0)
	registerModule(md)
	jitRoots = append(jitRoots, md, code)
	jitMu.Unlock()

	f := runtime.FuncForPC(uintptr(unsafe.Pointer(&code[0])))
	if f == nil {
		t.Fatal("FuncForPC returned nil for the injected function")
	}
	if got := f.Name(); got != "weave.jitstub" {
		t.Fatalf("FuncForPC name = %q, want weave.jitstub", got)
	}
	t.Logf("findfunc resolved injected trampoline: %s", f.Name())
}

// TestJITSelftest checks the C-level mmap+protect+execute round trip works at
// all, isolating platform mechanics from the trampoline machine code.
func TestJITSelftest(t *testing.T) {
	if runtime.GOARCH != "arm64" || runtime.GOOS != "darwin" {
		t.Skip("darwin/arm64 only")
	}
	if !rt.Selftest() {
		t.Fatal("C-level JIT selftest faulted")
	}
}
