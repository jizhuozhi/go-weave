package weave

// Precise trampolines: methods whose pointers travel through the caller's
// stack argument area. The generic trampoline describes that area as a byte
// window, so those methods are rejected unless a generated precise trampoline
// is registered for their shape (stubs_precise_arm64_test.go holds the ones
// these tests need, exactly as StubSource emits them).

import (
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// StackPtrs exercises the three interesting directions. Interface methods are
// ordered alphabetically, so the indices are Both=0, ManyPtrs=1, ManyStrs=2.
type StackPtrs interface {
	// Both spills pointer arguments and pointer results.
	Both(s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15 string) (
		t0, t1, t2, t3, t4, t5, t6, t7, t8, t9, t10, t11, t12, t13, t14, t15 string)
	// ManyPtrs spills pointer arguments only.
	ManyPtrs(s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15 string) int
	// ManyStrs spills pointer results only.
	ManyStrs(n int) (s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15 string)
}

type stackPtrsImpl struct{}

func (stackPtrsImpl) Both(s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15 string) (
	t0, t1, t2, t3, t4, t5, t6, t7, t8, t9, t10, t11, t12, t13, t14, t15 string) {
	in := []string{s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15}
	out := make([]string, 16)
	for i, s := range in {
		// Fresh allocations: a result the collector cannot see would be
		// unreachable the moment it is written into the caller's area.
		out[i] = strings.Repeat(s, 3)
	}
	return out[0], out[1], out[2], out[3], out[4], out[5], out[6], out[7],
		out[8], out[9], out[10], out[11], out[12], out[13], out[14], out[15]
}

func (stackPtrsImpl) ManyPtrs(s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15 string) int {
	n := 0
	for _, s := range []string{s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15} {
		n += len(s)
	}
	return n
}

func (stackPtrsImpl) ManyStrs(n int) (s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15 string) {
	out := make([]string, 16)
	for i := range out {
		out[i] = strings.Repeat("s", n+i)
	}
	return out[0], out[1], out[2], out[3], out[4], out[5], out[6], out[7],
		out[8], out[9], out[10], out[11], out[12], out[13], out[14], out[15]
}

var _ StackPtrs = stackPtrsImpl{}

// wantStrs is the 16 argument strings the tests pass, distinct in content and
// length so a misplaced word shows up as a wrong value rather than a crash.
func wantStrs() [16]string {
	var a [16]string
	for i := range a {
		a[i] = strings.Repeat(string(rune('a'+i)), i+1)
	}
	return a
}

// hasStub reports whether a precise trampoline is registered for method idx of
// t, i.e. whether this architecture has generated stubs checked in.
func hasStub(t reflect.Type, idx int) bool {
	l := newABILayout(t.Method(idx).Type)
	return !l.stackPointers() || lookupStub(l.shape(idx)) != nil
}

func requirePreciseStubs(t *testing.T) {
	t.Helper()
	it := reflect.TypeOf((*StackPtrs)(nil)).Elem()
	for i := 0; i < it.NumMethod(); i++ {
		if !hasStub(it, i) {
			t.Skipf("no precise trampolines generated for %s/%s; see StubSource",
				runtime.GOOS, runtime.GOARCH)
		}
	}
}

// TestPreciseStackPointers runs every direction through the precise
// trampolines, with a collector cycle inside the interceptor so that arguments
// and results are only reachable through the caller's stack argument area,
// described by the generated pointer map.
func TestPreciseStackPointers(t *testing.T) {
	requirePreciseStubs(t)

	churn := func(c *Invocation) []reflect.Value {
		runtime.GC()
		rets := c.Proceed()
		runtime.GC()
		return rets
	}
	p := New[StackPtrs](stackPtrsImpl{}, churn)
	a := wantStrs()

	t.Run("args", func(t *testing.T) {
		want := 0
		for _, s := range a {
			want += len(s)
		}
		for i := 0; i < 50; i++ {
			if got := p.ManyPtrs(a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7],
				a[8], a[9], a[10], a[11], a[12], a[13], a[14], a[15]); got != want {
				t.Fatalf("ManyPtrs = %d, want %d", got, want)
			}
		}
	})

	t.Run("results", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			out := collect16(p.ManyStrs(3))
			for k, s := range out {
				if want := strings.Repeat("s", 3+k); s != want {
					t.Fatalf("ManyStrs result %d = %q, want %q", k, s, want)
				}
			}
		}
	})

	t.Run("both", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			out := collect16(p.Both(a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7],
				a[8], a[9], a[10], a[11], a[12], a[13], a[14], a[15]))
			for k, s := range out {
				if want := strings.Repeat(a[k], 3); s != want {
					t.Fatalf("Both result %d = %q, want %q", k, s, want)
				}
			}
		}
	})
}

// TestPreciseStackPointersInterceptors reads and rewrites stack-assigned
// pointer arguments, which routes the call through the reflect fallback.
func TestPreciseStackPointersInterceptors(t *testing.T) {
	requirePreciseStubs(t)

	var seen string
	p := New[StackPtrs](stackPtrsImpl{}, func(c *Invocation) []reflect.Value {
		if c.Method.Name == "ManyPtrs" {
			seen = c.Arg(15).String()
			c.SetArg(15, reflect.ValueOf("rewritten"))
		}
		return c.Proceed()
	})
	a := wantStrs()
	want := len("rewritten")
	for _, s := range a[:15] {
		want += len(s)
	}
	if got := p.ManyPtrs(a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7],
		a[8], a[9], a[10], a[11], a[12], a[13], a[14], a[15]); got != want {
		t.Fatalf("ManyPtrs = %d, want %d", got, want)
	}
	if seen != a[15] {
		t.Fatalf("Arg(15) = %q, want %q", seen, a[15])
	}
}

// TestPreciseStackPointersConcurrent hammers the precise path from several
// goroutines, each with its own stack, so stack growth and moves happen while
// pointers sit in the caller's argument area.
func TestPreciseStackPointersConcurrent(t *testing.T) {
	requirePreciseStubs(t)

	p := New[StackPtrs](stackPtrsImpl{}, func(c *Invocation) []reflect.Value {
		return c.Proceed()
	})
	a := wantStrs()
	const gs = 16
	done := make(chan string, gs)
	for g := 0; g < gs; g++ {
		go func() {
			for i := 0; i < 200; i++ {
				out := collect16(p.Both(a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7],
					a[8], a[9], a[10], a[11], a[12], a[13], a[14], a[15]))
				for k, s := range out {
					if want := strings.Repeat(a[k], 3); s != want {
						done <- "Both result mismatch"
						return
					}
				}
			}
			done <- ""
		}()
	}
	for g := 0; g < gs; g++ {
		if msg := <-done; msg != "" {
			t.Fatal(msg)
		}
	}
}

func collect16(s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15 string) [16]string {
	return [16]string{s0, s1, s2, s3, s4, s5, s6, s7, s8, s9, s10, s11, s12, s13, s14, s15}
}

// TestPreciseCallerFrameIntact guards the one thing a precise trampoline writes
// on its own: the zeroing of the words the stack-assigned results will occupy.
// Those offsets are computed by the generator, and an offset past the area would
// land in the caller's frame — so the canary sits exactly there.
func TestPreciseCallerFrameIntact(t *testing.T) {
	requirePreciseStubs(t)

	p := New[StackPtrs](stackPtrsImpl{})
	a := wantStrs()
	var canary [64]uintptr
	for i := range canary {
		canary[i] = 0xc0de0000 + uintptr(i)
	}

	_ = p.ManyPtrs(a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7],
		a[8], a[9], a[10], a[11], a[12], a[13], a[14], a[15])
	_ = collect16(p.Both(a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7],
		a[8], a[9], a[10], a[11], a[12], a[13], a[14], a[15]))
	_ = collect16(p.ManyStrs(2))

	for k, v := range canary {
		if v != 0xc0de0000+uintptr(k) {
			t.Fatalf("caller frame corrupted at word %d: %#x", k, v)
		}
	}
}

// TestPreciseTemporariesKeepAlive passes freshly built strings straight into the
// call, so that the only reference to them the collector can be sure of is the
// one in the caller's stack argument area — described by the generated pointer
// map and by nothing else. A collection inside the interceptor then has to keep
// them alive.
func TestPreciseTemporariesKeepAlive(t *testing.T) {
	requirePreciseStubs(t)

	p := New[StackPtrs](stackPtrsImpl{}, func(c *Invocation) []reflect.Value {
		for i := 0; i < 4; i++ {
			runtime.GC()
			runtime.Gosched()
		}
		return c.Proceed()
	})

	for i := 0; i < 100; i++ {
		// mk allocates a fresh string every time: no other copy of it exists.
		mk := func(n int) string { return strings.Repeat("q", n) }
		want := 0
		for n := 1; n <= 16; n++ {
			want += n
		}
		if got := p.ManyPtrs(mk(1), mk(2), mk(3), mk(4), mk(5), mk(6), mk(7), mk(8),
			mk(9), mk(10), mk(11), mk(12), mk(13), mk(14), mk(15), mk(16)); got != want {
			t.Fatalf("ManyPtrs over temporaries = %d, want %d", got, want)
		}
	}
}

// TestShape pins the shapes of the three methods: they are what the generated
// trampolines claim, and a change here means the checked-in stubs are stale.
func TestShape(t *testing.T) {
	it := reflect.TypeOf((*StackPtrs)(nil)).Elem()
	for i := 0; i < it.NumMethod(); i++ {
		m := it.Method(i)
		l := newABILayout(m.Type)
		if !l.stackPointers() {
			t.Errorf("%s: expected pointers in the stack argument area", m.Name)
			continue
		}
		sh := l.shape(i)
		// Pointer words must be exactly the string data words: every other
		// word from the start of each region, since a string is (data, len).
		if sh.argWords%2 != 0 || sh.retWords%2 != 0 {
			t.Errorf("%s: odd word counts in %s", m.Name, sh)
		}
		t.Logf("%s: %s", m.Name, sh)
	}
}

// TestStubSourceMatchesRegistry generates the source for StackPtrs and checks
// that it declares exactly the shapes the interface needs — the guard against
// the generator and the layout drifting apart.
func TestStubSourceMatchesRegistry(t *testing.T) {
	it := reflect.TypeOf((*StackPtrs)(nil)).Elem()
	src := stubSource("weave", "", "", it)
	for i := 0; i < it.NumMethod(); i++ {
		l := newABILayout(it.Method(i).Type)
		sh := l.shape(i)
		if !strings.Contains(src, "func "+sh.name()+"(") {
			t.Errorf("generated source has no trampoline for %s (%s)", it.Method(i).Name, sh)
		}
		if !strings.Contains(src, "Func: "+sh.name()) {
			t.Errorf("generated source does not register %s", sh.name())
		}
	}
	if !strings.Contains(src, "//go:build "+runtime.GOARCH) {
		t.Error("generated source has no build constraint for the current architecture")
	}
	// The generated file must be self-contained Go: an unqualified in-package
	// generation calls Dispatch and RegisterStub directly.
	for _, want := range []string{"//go:nosplit", "//go:norace", "return Dispatch(", "RegisterStub(StubSpec{"} {
		if !strings.Contains(src, want) {
			t.Errorf("generated source is missing %q", want)
		}
	}
}

// TestStubSourceEmptyForRegisterOnlyInterface: an interface that keeps its
// pointers in registers needs no generated code at all.
func TestStubSourceEmptyForRegisterOnlyInterface(t *testing.T) {
	src := stubSource("app", weaveImportPath, "weave", reflect.TypeOf((*Service)(nil)).Elem())
	if strings.Contains(src, "func weaveStub_") {
		t.Errorf("Service needs no precise trampoline, got:\n%s", src)
	}
	if strings.Contains(src, "import") {
		t.Error("the empty file should not import anything")
	}
}

// TestRegisterStubValidatesShape: a trampoline whose signature does not match
// the shape it claims would hand the collector a wrong pointer map, so
// registration rejects it.
func TestRegisterStubValidatesShape(t *testing.T) {
	cases := []struct {
		name string
		spec StubSpec
	}{
		{"not a function", StubSpec{Func: 42}},
		{"wrong arity", StubSpec{ArgWords: 2, ArgPtrs: 0b01, Func: func() {}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a panic")
				}
			}()
			RegisterStub(tc.spec)
		})
	}
}
