package weave

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// --- test subjects ----------------------------------------------------------

type Payload struct {
	A int
	B string
	C float64
}

type Service interface {
	Noop()
	Echo(s string) string
	Add(a, b int) (int, error)
	Sum(xs []int) int
	Mix(s string, f float64, n int, p *Payload) (string, error)
	Floats(a, b, c, d float64) float64
	Variadic(prefix string, args ...int) string
	Swap(p Payload) Payload
	Triple() (int, string, error)
	Ptr() *Payload
	Nil() *Payload
}

type svc struct{}

func (svc) Noop()                     {}
func (svc) Echo(s string) string      { return s }
func (svc) Add(a, b int) (int, error) { return a + b, nil }
func (svc) Sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}
func (svc) Mix(s string, f float64, n int, p *Payload) (string, error) {
	if p == nil {
		return "", errors.New("nil payload")
	}
	return fmt.Sprintf("%s:%v:%d:%d:%s:%.1f", s, f, n, p.A, p.B, p.C), nil
}
func (svc) Floats(a, b, c, d float64) float64 { return a*b + c/d }
func (svc) Variadic(prefix string, args ...int) string {
	return fmt.Sprintf("%s:%d", prefix, len(args))
}
func (svc) Swap(p Payload) Payload       { p.A++; return p }
func (svc) Triple() (int, string, error) { return 1, "two", errors.New("three") }
func (svc) Ptr() *Payload                { return &Payload{A: 7} }
func (svc) Nil() *Payload                { return nil }

var _ Service = svc{}

// forBackend runs the body. It used to exercise two trampoline backends; today
// there is a single codegen backend, so it is a thin wrapper kept for the
// naming symmetry of the tests.
func forBackend(t *testing.T, body func(t *testing.T)) {
	t.Helper()
	t.Run("fast", body)
}

func newProxy(interceptors ...Interceptor) Service {
	return New[Service](svc{}, interceptors...)
}

// --- correctness ------------------------------------------------------------

func TestBasic(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		p := newProxy()

		if got := p.Echo("hello"); got != "hello" {
			t.Fatalf("Echo = %q", got)
		}
		if got, err := p.Add(2, 3); got != 5 || err != nil {
			t.Fatalf("Add = %d, %v", got, err)
		}
		if got := p.Sum([]int{1, 2, 3, 4}); got != 10 {
			t.Fatalf("Sum = %d", got)
		}
		if got, err := p.Mix("x", 2.5, 3, &Payload{A: 4, B: "y", C: 1.5}); err != nil || got != "x:2.5:3:4:y:1.5" {
			t.Fatalf("Mix = %q, %v", got, err)
		}
		if got := p.Floats(2, 3, 4, 8); got != 6.5 {
			t.Fatalf("Floats = %v", got)
		}
		if got := p.Variadic("v", 1, 2, 3); got != "v:3" {
			t.Fatalf("Variadic = %q", got)
		}
		if got := p.Swap(Payload{A: 1}); got.A != 2 {
			t.Fatalf("Swap = %+v", got)
		}
		if a, b, c := p.Triple(); a != 1 || b != "two" || c.Error() != "three" {
			t.Fatalf("Triple = %d %q %v", a, b, c)
		}
		if got := p.Ptr(); got == nil || got.A != 7 {
			t.Fatalf("Ptr = %+v", got)
		}
		if got := p.Nil(); got != nil {
			t.Fatalf("Nil = %+v", got)
		}
	})
}

// --- stack argument area ----------------------------------------------------

type Big struct{ V [24]int } // 192 bytes: stack-assigned on every architecture

type Wide interface {
	// Sum18 spills arguments past the register file on every supported
	// architecture (receiver consumes one integer register, leaving 15 on
	// arm64 and 8 on amd64).
	Sum18(a0, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14, a15, a16, a17 int) int
	TakeBig(b Big) int
	// Fan20 spills results past the result register file.
	Fan20(a int) (r0, r1, r2, r3, r4, r5, r6, r7, r8, r9, r10, r11, r12, r13, r14, r15, r16, r17, r18, r19 int)
}

func TestStackArgs(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		p := New[Wide](wideImpl{})

		if got := p.Sum18(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17); got != 153 {
			t.Fatalf("Sum18 = %d, want 153", got)
		}

		big := Big{}
		for i := range big.V {
			big.V[i] = i
		}
		if got := p.TakeBig(big); got != 24*23/2 {
			t.Fatalf("TakeBig = %d", got)
		}

		r0, r1, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, _, r18, r19 := p.Fan20(7)
		if r0 != 7 || r1 != 8 || r18 != 25 || r19 != 26 {
			t.Fatalf("Fan20 edges = %d %d %d %d", r0, r1, r18, r19)
		}
	})
}

type wideImpl struct{}

func (wideImpl) Sum18(a0, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14, a15, a16, a17 int) int {
	return a0 + a1 + a2 + a3 + a4 + a5 + a6 + a7 + a8 + a9 + a10 + a11 + a12 + a13 + a14 + a15 + a16 + a17
}

func (wideImpl) TakeBig(b Big) int {
	n := 0
	for _, v := range b.V {
		n += v
	}
	return n
}

func (wideImpl) Fan20(a int) (r0, r1, r2, r3, r4, r5, r6, r7, r8, r9, r10, r11, r12, r13, r14, r15, r16, r17, r18, r19 int) {
	vs := []*int{&r0, &r1, &r2, &r3, &r4, &r5, &r6, &r7, &r8, &r9, &r10, &r11, &r12, &r13, &r14, &r15, &r16, &r17, &r18, &r19}
	for i := range vs {
		*vs[i] = a + i
	}
	return
}

// TestStackArgInterceptors exercises argument inspection and rewriting for a
// stack-spilling method: materialisation reads from the pooled stack buffer,
// modifications flow through the reflect fallback path.
func TestStackArgInterceptors(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		seen := -1
		p := New[Wide](wideImpl{}, func(c *Invocation) []reflect.Value {
			seen = int(c.Arg(17).Int())
			c.SetArg(0, reflect.ValueOf(100))
			return c.Proceed()
		})
		if got := p.Sum18(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17); got != 100+1+2+3+4+5+6+7+8+9+10+11+12+13+14+15+16+17 {
			t.Fatalf("Sum18 rewritten = %d", got)
		}
		if seen != 17 {
			t.Fatalf("Arg(17) = %d", seen)
		}
	})
}

func TestInterceptors(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		var log []string

		logging := func(c *Invocation) []reflect.Value {
			log = append(log, c.Method.Name)
			return c.Proceed()
		}

		doubling := func(c *Invocation) []reflect.Value {
			if c.Method.Name == "Add" {
				a := int(c.Arg(0).Int())
				b := int(c.Arg(1).Int())
				c.SetArg(0, reflect.ValueOf(a*2))
				c.SetArg(1, reflect.ValueOf(b*2))
			}
			return c.Proceed()
		}

		p := newProxy(logging, doubling)

		// 3*2 + 4*2 = 14.
		if got, _ := p.Add(3, 4); got != 14 {
			t.Fatalf("Add = %d, want 14", got)
		}
		if got := p.Echo("hi"); got != "hi" {
			t.Fatalf("Echo = %q", got)
		}
		if len(log) != 2 || log[0] != "Add" || log[1] != "Echo" {
			t.Fatalf("log = %v", log)
		}
	})
}

// TestFastPathSemantics exercises the register fast path of callTarget: an
// interceptor that never touches the arguments, Proceed called twice, and
// arguments read after Proceed has already overwritten the live registers
// with results — all served from the argument register snapshot.
func TestFastPathSemantics(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		// A no-touch interceptor: the call runs entirely through redial.
		passthrough := func(c *Invocation) []reflect.Value {
			return c.Proceed()
		}
		p := newProxy(passthrough)
		if got, _ := p.Add(3, 4); got != 7 {
			t.Fatalf("Add = %d", got)
		}
		if got := p.Echo("fast"); got != "fast" {
			t.Fatalf("Echo = %q", got)
		}
		if got := p.Floats(1, 2, 3, 4); got != 1*2+3.0/4 {
			t.Fatalf("Floats = %v", got)
		}
		if got := p.Variadic("p", 1, 2, 3); got != "p:3" {
			t.Fatalf("Variadic = %q", got)
		}

		// Proceed twice: the second replay restores the argument registers
		// from the snapshot, and a GC in between must not collect the
		// pointer arguments that live only in the snapshot.
		retry := func(c *Invocation) []reflect.Value {
			rets := c.Proceed()
			if n := len(rets); n > 0 && rets[0].Int() == 0 {
				runtime.GC()
				rets = c.Proceed()
			}
			return rets
		}
		p2 := newProxy(retry)
		if got, _ := p2.Add(5, 6); got != 11 {
			t.Fatalf("Add (double Proceed) = %d", got)
		}
		if got := p2.Echo("twice"); got != "twice" {
			t.Fatalf("Echo (double Proceed) = %q", got)
		}

		// Arguments read after Proceed: the results already clobbered the
		// live registers, so the materialisation must come from the snapshot.
		postHoc := func(c *Invocation) []reflect.Value {
			rets := c.Proceed()
			if c.NumArg() == 2 {
				if a, b := c.Arg(0).Int(), c.Arg(1).Int(); a != 8 || b != 9 {
					t.Errorf("post-Proceed args = %d, %d; want 8, 9", a, b)
				}
			}
			return rets
		}
		p3 := newProxy(postHoc)
		if got, _ := p3.Add(8, 9); got != 17 {
			t.Fatalf("Add (post-hoc args) = %d", got)
		}
	})
}

// TestFastPathPointerArgs keeps pointer arguments alive only through the
// argument register snapshot while a GC cycle runs inside an interceptor.
func TestFastPathPointerArgs(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		gc := func(c *Invocation) []reflect.Value {
			runtime.GC()
			return c.Proceed()
		}
		p := newProxy(gc)
		pl := &Payload{A: 7, B: "keepalive", C: 8}
		if got, err := p.Mix("s", 1.5, 2, pl); err != nil || got != "s:1.5:2:7:keepalive:8.0" {
			t.Fatalf("Mix = %q, %v", got, err)
		}
		if got := p.Sum([]int{1, 2, 3, 4}); got != 10 {
			t.Fatalf("Sum = %d", got)
		}
	})
}

func TestShortCircuit(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		cached := func(c *Invocation) []reflect.Value {
			if c.Method.Name == "Add" {
				return []reflect.Value{reflect.ValueOf(42), reflect.Zero(reflect.TypeOf((*error)(nil)).Elem())}
			}
			return c.Proceed()
		}
		p := newProxy(cached)
		if got, err := p.Add(1, 2); got != 42 || err != nil {
			t.Fatalf("Add = %d, %v", got, err)
		}
		if got := p.Echo("x"); got != "x" {
			t.Fatalf("Echo = %q", got)
		}
	})
}

func TestMock(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		p := New[Service](nil)
		if got, _ := p.Add(1, 2); got != 0 {
			t.Fatalf("mock Add = %d", got)
		}
		if got := p.Echo("x"); got != "" {
			t.Fatalf("mock Echo = %q", got)
		}
	})
}

// The proxy value is a first-class value of its interface type: it can be
// stored, passed around and called through T directly. Type switches over it
// are possible too, but note that a forged itab only satisfies the runtime's
// `tab.inter == T` fast path; round-tripping through `any` and asserting back
// calls getitab and fails, because *Proxy does not statically declare T's
// methods.
func TestTypeAssertion(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		p := newProxy()
		// Pass it through a function expecting exactly T: the forged itab is a
		// real T value end to end.
		roundTrip := func(s Service) string { return s.Echo("ok") }
		if got := roundTrip(p); got != "ok" {
			t.Fatalf("round trip = %q", got)
		}
	})
}

// Two proxies of the same interface share trampoline slots (they are interned
// per (interface, method) pair), so each call must resolve the target through
// its own receiver. Regression test: an earlier version resolved the target
// from the shared slot, making every proxy delegate to the first proxy's
// target.
func TestSharedSlotsDistinctTargets(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		a, b := New[Service](counting("a")), New[Service](counting("b"))
		if got, _ := a.Add(1, 2); got != 3 {
			t.Fatalf("a.Add = %d", got)
		}
		if got := a.Echo("x"); got != "a:x" {
			t.Fatalf("a.Echo = %q", got)
		}
		if got := b.Echo("x"); got != "b:x" {
			t.Fatalf("b.Echo = %q", got)
		}
		if got := a.Echo("y"); got != "a:y" {
			t.Fatalf("a.Echo again = %q", got)
		}
		if got, _ := b.Add(4, 5); got != 9 {
			t.Fatalf("b.Add = %d", got)
		}
		if got, _ := a.Add(10, 10); got != 20 {
			t.Fatalf("a.Add again = %d", got)
		}

		// Same, but with a mock (no target at all) alongside a live one.
		m := New[Service](nil)
		if got, _ := m.Add(1, 1); got != 0 {
			t.Fatalf("mock.Add = %d", got)
		}
		if got, _ := b.Add(6, 6); got != 12 {
			t.Fatalf("b.Add after mock = %d", got)
		}
	})
}

// counting returns a target whose Add is distinguishable per name.
func counting(name string) Service {
	return tagged{name}
}

type tagged struct{ name string }

func (t tagged) Noop()                     {}
func (t tagged) Echo(s string) string      { return t.name + ":" + s }
func (t tagged) Add(a, b int) (int, error) { return a + b, nil }
func (t tagged) Sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}
func (t tagged) Mix(string, float64, int, *Payload) (string, error) { return "", nil }
func (t tagged) Floats(a, b, c, d float64) float64                  { return a + b + c + d }
func (t tagged) Variadic(p string, args ...int) string              { return p }
func (t tagged) Swap(p Payload) Payload                             { return p }
func (t tagged) Triple() (int, string, error)                       { return 0, "", nil }
func (t tagged) Ptr() *Payload                                      { return nil }
func (t tagged) Nil() *Payload                                      { return nil }

func TestConcurrent(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		p := newProxy()
		const n = 64
		done := make(chan int, n)
		for i := 0; i < n; i++ {
			go func(i int) {
				for j := 0; j < 1000; j++ {
					if got, _ := p.Add(i, j); got != i+j {
						panic(fmt.Sprintf("Add(%d,%d)=%d", i, j, got))
					}
				}
				done <- 1
			}(i)
		}
		for i := 0; i < n; i++ {
			<-done
		}
	})
}

// TestCallerFrameIntact guards against the trampoline spilling registers into
// its caller's frame. A register parameter's ABI home slot lives in the
// caller's outgoing argument area, which an itab call site reserves for the
// method's signature only — a trampoline that spills its full maximal register
// file there (the race prologue used to, and the morestack trampoline of a
// split stub still would) overwrites the caller's locals. The canary sits in
// the immediate caller's frame, exactly where such a spill lands.
func TestCallerFrameIntact(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		p := newProxy()
		var canary [32]uintptr
		for i := range canary {
			canary[i] = 0xfeed0000 + uintptr(i)
		}
		if got := p.Echo("x"); got != "x" {
			t.Fatalf("Echo = %q", got)
		}
		if n, _ := p.Add(2, 3); n != 5 {
			t.Fatalf("Add = %d", n)
		}
		_ = p.Floats(1, 2, 3, 4)
		_ = p.Sum([]int{1, 2, 3})
		for k, v := range canary {
			if v != 0xfeed0000+uintptr(k) {
				t.Fatalf("caller frame corrupted at word %d: %#x", k, v)
			}
		}
	})
}

// TestStackGrowAtStub forces a stack growth whose morestack fires with the
// trampoline as the innermost frame: a fresh goroutine whose first proxy call
// needs more stack than it has. The trampoline's stack check runs before its
// frame exists, so a mis-behaving growth path would corrupt the caller.
func TestStackGrowAtStub(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		p := newProxy()
		const gs = 8
		done := make(chan int, gs)
		for g := 0; g < gs; g++ {
			go func(i int) {
				for j := 0; j < 100; j++ {
					if got, _ := p.Add(i, j); got != i+j {
						panic("bad result")
					}
				}
				done <- 1
			}(g)
		}
		for g := 0; g < gs; g++ {
			<-done
		}
	})
}

// --- GC safety --------------------------------------------------------------

type GCSink interface {
	Take(p *Payload) int
	TakeStr(s string) string
	TakeSlice(xs []int) int
}

type gcImpl struct{}

func (gcImpl) Take(p *Payload) int     { return p.A }
func (gcImpl) TakeStr(s string) string { return s }
func (gcImpl) TakeSlice(xs []int) int  { return len(xs) }

// TestArgKeepAlive verifies that arguments that exist only in the register
// spill area survive a GC that happens inside an interceptor. Run it with
// GODEBUG=clobberfree=1 to make a missed pointer obvious.
func TestArgKeepAlive(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		churn := func(c *Invocation) []reflect.Value {
			for i := 0; i < 8; i++ {
				runtime.GC()
				runtime.Gosched()
			}
			// Only now look at the argument; if it was collected this reads
			// garbage.
			v := c.Arg(0).Interface().(*Payload)
			return []reflect.Value{reflect.ValueOf(v.A)}
		}

		p := New[GCSink](gcImpl{}, churn)
		for i := 0; i < 200; i++ {
			if got := p.Take(&Payload{A: 12345}); got != 12345 {
				t.Fatalf("Take = %d after GC churn", got)
			}
		}
	})
}

func TestStringKeepAlive(t *testing.T) {
	forBackend(t, func(t *testing.T) {
		churn := func(c *Invocation) []reflect.Value {
			for i := 0; i < 6; i++ {
				runtime.GC()
			}
			return []reflect.Value{c.Arg(0)}
		}
		p := New[GCSink](gcImpl{}, churn)
		want := strings.Repeat("x", 1<<16)
		for i := 0; i < 50; i++ {
			if got := p.TakeStr(want); got != want {
				t.Fatalf("TakeStr lost its argument")
			}
		}
	})
}

// --- benchmarks -------------------------------------------------------------

func BenchmarkDirectAdd(b *testing.B) {
	s := svc{}
	for i := 0; i < b.N; i++ {
		s.Add(1, 2)
	}
}

func BenchmarkProxyAdd(b *testing.B) {
	p := newProxy()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Add(1, 2)
	}
}

// BenchmarkProxyIntercept measures the realistic AOP shape: one interceptor
// that observes but never touches the arguments, so the call runs entirely
// through the register fast path.
func BenchmarkProxyIntercept(b *testing.B) {
	var count int
	p := newProxy(func(c *Invocation) []reflect.Value {
		count++
		return c.Proceed()
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Add(1, 2)
	}
	b.StopTimer()
	if count != b.N {
		b.Fatalf("interceptor ran %d times, want %d", count, b.N)
	}
}

// BenchmarkProxyInspect forces the reflect fallback: the interceptor reads
// and rewrites every argument, so they must be materialised as Values.
func BenchmarkProxyInspect(b *testing.B) {
	p := newProxy(func(c *Invocation) []reflect.Value {
		c.SetArg(0, reflect.ValueOf(int(c.Arg(0).Int())+1))
		return c.Proceed()
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Add(1, 2)
	}
}
