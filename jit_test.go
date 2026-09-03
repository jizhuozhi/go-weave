//go:build (amd64 || arm64) && go1.23

package weave

import (
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/jizhuozhi/go-weave/internal/rt"
)

// jitOnly is a pointer-spilling interface with no compile-time trampoline
// registered, so proxying it exercises the runtime JIT path end to end.
type jitOnly interface {
	Spill(s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15 string) int
}

type jitOnlyImpl struct{}

func (jitOnlyImpl) Spill(s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15 string) int {
	n := 0
	for _, s := range []string{s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15} {
		n += len(s)
	}
	return n
}

// TestJITFindfunc verifies the layout mirror is exact and a registered module
// is discoverable by runtime.FuncForPC.
func TestJITFindfunc(t *testing.T) {
	// Sanity: the mirror's field offsets must agree with the runtime's.
	if got := unsafe.Offsetof(lastmoduledatap.text); got != 176 {
		t.Fatalf("moduledata.text offset = %d, want 176", got)
	}
	if got := unsafe.Offsetof(lastmoduledatap.gofunc); got != 320 {
		t.Fatalf("moduledata.gofunc offset = %d, want 320", got)
	}
	if got := unsafe.Offsetof(lastmoduledatap.next); got != 576 {
		t.Fatalf("moduledata.next offset = %d, want 576", got)
	}
	if got := unsafe.Offsetof(lastmoduledatap.pctab); got != 80 {
		t.Fatalf("moduledata.pctab offset = %d, want 80", got)
	}

	// This first step only probes findfunc, so a plain writable mapping stands
	// in for the text range — findmoduledatap matches on [minpc, maxpc) and
	// never checks executability. (Real execution needs MAP_JIT +
	// pthread_jit_write_protect_np on Apple Silicon, handled in the next step.)
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

// TestJITExec proves the JIT trampoline actually runs end to end: it dispatches
// method 5 (Noop) of a Service proxy through the generated machine code.
func TestJITExec(t *testing.T) {
	p := New[Service](svc{})
	entry := jitTrampoline(stubShape{index: 5})
	if entry == nil {
		t.Skip("JIT unavailable")
	}

	proxy := (*Proxy)((*iface)(unsafe.Pointer(&p)).data)
	funs := unsafe.Slice(&proxy.itab.Fun[0], len(proxy.methods))
	funs[5] = uintptr(entry)
	p.Noop()
	// Reaching here without a fault means the JIT trampoline ran Dispatch.
	t.Log("JIT trampoline dispatched Noop")
}

// TestJITStackPtrs routes ManyPtrs — whose pointer arguments spill past the
// register file onto the stack — through a JIT trampoline and runs collector
// cycles inside the interceptor. The argument strings are fresh allocations
// whose only live reference during the call is the caller's stack argument
// area, so the test fails if the JIT argument map does not keep them alive:
// under GODEBUG=clobberfree=1 a collected word reads back as garbage.
func TestJITStackPtrs(t *testing.T) {
	it := reflect.TypeOf((*StackPtrs)(nil)).Elem()
	l := newABILayout(it.Method(1).Type) // ManyPtrs
	if !l.stackPointers() {
		t.Fatalf("ManyPtrs must spill pointer arguments on %s", runtime.GOARCH)
	}
	sh := l.shape(1)

	entry := jitTrampoline(sh)
	if entry == nil {
		t.Skipf("JIT unavailable on %s", runtime.GOARCH)
	}

	p := New[StackPtrs](stackPtrsImpl{}, func(c *Invocation) []reflect.Value {
		runtime.GC()
		runtime.GC()
		return c.Proceed()
	})

	proxy := (*Proxy)((*iface)(unsafe.Pointer(&p)).data)
	funs := unsafe.Slice(&proxy.itab.Fun[0], len(proxy.methods))
	funs[sh.index] = uintptr(entry)

	for i := 0; i < 200; i++ {
		mk := func(n int) string { return strings.Repeat("q", n) }
		want := 0
		for n := 1; n <= 16; n++ {
			want += n
		}
		got := p.ManyPtrs(mk(1), mk(2), mk(3), mk(4), mk(5), mk(6), mk(7), mk(8),
			mk(9), mk(10), mk(11), mk(12), mk(13), mk(14), mk(15), mk(16))
		if got != want {
			t.Fatalf("ManyPtrs over temporaries = %d, want %d (iteration %d)", got, want, i)
		}
	}
}

// TestJITProxyFallback proxies an interface with no compile-time trampoline, so
// New itself generates the trampoline via the JIT fallback — the end-to-end
// path users get without StubSource — and verifies the result is correct under
// collector cycles.
func TestJITProxyFallback(t *testing.T) {
	it := reflect.TypeOf((*jitOnly)(nil)).Elem()
	l := newABILayout(it.Method(0).Type)
	if !l.stackPointers() {
		t.Fatalf("Spill must move pointers through the stack on %s", runtime.GOARCH)
	}
	if code := lookupStub(l.shape(0)); code != nil {
		t.Fatal("jitOnly.Spill unexpectedly has a compile-time trampoline")
	}

	p := New[jitOnly](jitOnlyImpl{}, func(c *Invocation) []reflect.Value {
		runtime.GC()
		runtime.GC()
		return c.Proceed()
	})

	for i := 0; i < 200; i++ {
		mk := func(n int) string { return strings.Repeat("q", n) }
		want := 0
		for n := 1; n <= 16; n++ {
			want += n
		}
		got := p.Spill(mk(1), mk(2), mk(3), mk(4), mk(5), mk(6), mk(7), mk(8),
			mk(9), mk(10), mk(11), mk(12), mk(13), mk(14), mk(15), mk(16))
		if got != want {
			t.Fatalf("Spill over temporaries = %d, want %d (iteration %d)", got, want, i)
		}
	}
}
