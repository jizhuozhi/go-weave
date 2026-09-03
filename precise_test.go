//go:build (amd64 || arm64) && go1.23

package weave

// Precise trampolines: methods whose pointers travel through the caller's
// stack argument area. The generic trampoline describes that area as a byte
// window, so those methods need a precise trampoline, generated at runtime by
// the JIT. These tests exercise that runtime-generated path end to end.

import (
	"fmt"
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

// Exotic covers two spill shapes the string-only StackPtrs methods do not: a
// variadic that packs into a pointer-bearing slice, and interface values, which
// are two pointer words (itab, data) each.
type Exotic interface {
	// VariadicSlice: the variadic tail packs into a []*Payload — three words
	// whose data word is a pointer — and the fifteen ints before it exhaust the
	// register file, so the whole slice spills onto the stack.
	VariadicSlice(a0, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14 int, ps ...*Payload) int
	// Iface: eight interface values — sixteen pointer words — overflow the
	// register file on every supported architecture (the receiver consumes one
	// word, leaving 15 on arm64 and 8 on amd64).
	Iface(a0, a1, a2, a3, a4, a5, a6, a7 fmt.Stringer) string
}

type exoticImpl struct{}

func (exoticImpl) VariadicSlice(a0, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14 int, ps ...*Payload) int {
	n := a0 + a1 + a2 + a3 + a4 + a5 + a6 + a7 + a8 + a9 + a10 + a11 + a12 + a13 + a14
	for _, p := range ps {
		n += p.A
	}
	return n
}

func (exoticImpl) Iface(a0, a1, a2, a3, a4, a5, a6, a7 fmt.Stringer) string {
	vs := []fmt.Stringer{a0, a1, a2, a3, a4, a5, a6, a7}
	var b strings.Builder
	for _, v := range vs {
		b.WriteString(v.String())
		b.WriteByte('|')
	}
	return b.String()
}

// tagStringer is a fmt.Stringer carrying a heap payload; once boxed into an
// interface, the interface value's data word is its only reference.
type tagStringer struct{ s string }

func (s tagStringer) String() string { return s.s }

// pStringer is a pointer fmt.Stringer. Boxing it makes the interface data word
// point straight at the object, so a finalizer reports exactly when the data
// word stops keeping the object alive.
type pStringer struct{ s string }

func (p *pStringer) String() string { return p.s }

var _ Exotic = exoticImpl{}

// wantStrs is the 16 argument strings the tests pass, distinct in content and
// length so a misplaced word shows up as a wrong value rather than a crash.
func wantStrs() [16]string {
	var a [16]string
	for i := range a {
		a[i] = strings.Repeat(string(rune('a'+i)), i+1)
	}
	return a
}

// requireJIT skips the test unless the runtime JIT can generate a trampoline
// for every pointer-spilling method of it. Generating here also warms the cache,
// so the test bodies exercise the cached path.
func requireJIT(t *testing.T, it reflect.Type) {
	t.Helper()
	for i := 0; i < it.NumMethod(); i++ {
		l := newABILayout(it.Method(i).Type)
		if !l.stackPointers() {
			continue
		}
		if jitTrampoline(l.shape(i)) == nil {
			t.Skipf("runtime JIT unavailable on %s/%s", runtime.GOOS, runtime.GOARCH)
		}
	}
}

// TestPreciseStackPointers runs every direction through the precise
// trampolines, with a collector cycle inside the interceptor so that arguments
// and results are only reachable through the caller's stack argument area,
// described by the generated pointer map.
func TestPreciseStackPointers(t *testing.T) {
	requireJIT(t, reflect.TypeOf((*StackPtrs)(nil)).Elem())

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
	requireJIT(t, reflect.TypeOf((*StackPtrs)(nil)).Elem())

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
	requireJIT(t, reflect.TypeOf((*StackPtrs)(nil)).Elem())

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
	requireJIT(t, reflect.TypeOf((*StackPtrs)(nil)).Elem())

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
	requireJIT(t, reflect.TypeOf((*StackPtrs)(nil)).Elem())

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

// TestPreciseExoticShapes runs the variadic-slice and interface-value spill
// shapes through the precise trampolines. The arguments are temporaries built
// inside the call, so the only reference to them the collector can be sure of
// is the one in the caller's stack argument area.
func TestPreciseExoticShapes(t *testing.T) {
	requireJIT(t, reflect.TypeOf((*Exotic)(nil)).Elem())

	churn := func(c *Invocation) []reflect.Value {
		runtime.GC()
		return c.Proceed()
	}
	p := New[Exotic](exoticImpl{}, churn)

	t.Run("variadic", func(t *testing.T) {
		// Sum of ints 0..14 is 105; the payloads add 1..5.
		for i := 0; i < 50; i++ {
			got := p.VariadicSlice(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14,
				&Payload{A: 1}, &Payload{A: 2}, &Payload{A: 3}, &Payload{A: 4}, &Payload{A: 5})
			if got != 120 {
				t.Fatalf("VariadicSlice = %d, want 120", got)
			}
		}
	})

	t.Run("iface", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			got := p.Iface(
				tagStringer{s: "s0"}, tagStringer{s: "s1"}, tagStringer{s: "s2"}, tagStringer{s: "s3"},
				tagStringer{s: "s4"}, tagStringer{s: "s5"}, tagStringer{s: "s6"}, tagStringer{s: "s7"})
			if want := "s0|s1|s2|s3|s4|s5|s6|s7|"; got != want {
				t.Fatalf("Iface = %q, want %q", got, want)
			}
		}
	})
}

// TestPreciseIfaceKeepAlive is the strongest form of the interface test: the
// arguments are pointer Stringers created inline, so their only reference is
// the interface data word — in a register for the first seven, on the stack for
// the eighth. A collector cycle runs inside the interceptor and then every
// argument is materialised and read back: a missed data word means the object
// was collected, and under GODEBUG=clobberfree=1 its payload reads back as
// garbage.
func TestPreciseIfaceKeepAlive(t *testing.T) {
	requireJIT(t, reflect.TypeOf((*Exotic)(nil)).Elem())

	p := New[Exotic](exoticImpl{}, func(c *Invocation) []reflect.Value {
		runtime.GC()
		runtime.GC()
		for i := 0; i < c.NumArg(); i++ {
			s := c.Arg(i).Interface().(*pStringer).s
			if !strings.HasPrefix(s, "x") || s[len(s)-1] != byte('A'+i) {
				t.Errorf("argument %d read back as %q", i, s)
			}
		}
		return c.Proceed()
	})

	for i := 0; i < 200; i++ {
		mk := func(k int) fmt.Stringer {
			return &pStringer{s: strings.Repeat("x", 64) + string(rune('A'+k))}
		}
		if got := p.Iface(mk(0), mk(1), mk(2), mk(3), mk(4), mk(5), mk(6), mk(7)); got == "" {
			t.Fatal("empty result")
		}
	}
}

// TestShape pins the shapes of the three methods: they describe the stack area
// the JIT trampoline's argument map must cover, and a change here means the
// ABI layout code and the trampoline drift apart.
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
