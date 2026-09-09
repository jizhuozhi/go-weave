package mockito

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

type Greeter interface {
	Hello(name string) string
	Greet(name string) (string, error)
	Triple(name string) (string, int, error)
	Quad(name string) (string, int, bool, error)
}

type realGreeter struct{}

func (realGreeter) Hello(name string) string { return "hello, " + name }
func (realGreeter) Greet(name string) (string, error) {
	return "greet, " + name, nil
}
func (realGreeter) Triple(name string) (string, int, error) {
	return "triple, " + name, 3, nil
}
func (realGreeter) Quad(name string) (string, int, bool, error) {
	return "quad, " + name, 4, true, nil
}

func TestMockReturnsZero(t *testing.T) {
	m := Mock[Greeter]()
	if got := m.Hello("ada"); got != "" {
		t.Fatalf("mock returned %q, want zero", got)
	}
	if n := Verify(m.Hello("ada")).Count(); n != 1 {
		t.Fatalf("recorded %d calls, want 1", n)
	}
}

func TestStubReturn(t *testing.T) {
	m := Mock[Greeter]()
	When(m.Hello("ada")).ThenReturn("hi ada")

	if got := m.Hello("ada"); got != "hi ada" {
		t.Fatalf("got %q, want %q", got, "hi ada")
	}
	if got := m.Hello("bob"); got != "" {
		t.Fatalf("unstubbed call got %q, want zero", got)
	}
}

func TestStubThenAnswer(t *testing.T) {
	m := Mock[Greeter]()
	When(m.Hello("ada")).ThenAnswer(func(args []any) []any {
		return []any{"hi " + args[0].(string)}
	})

	if got := m.Hello("ada"); got != "hi ada" {
		t.Fatalf("got %q, want %q", got, "hi ada")
	}
}

func TestStubMultiReturn(t *testing.T) {
	m := Mock[Greeter]()
	When2(m.Greet("ada")).ThenReturn("hi ada", nil)

	got, err := m.Greet("ada")
	if got != "hi ada" || err != nil {
		t.Fatalf("got (%q, %v)", got, err)
	}
}

func TestVerify(t *testing.T) {
	m := Mock[Greeter]()
	m.Hello("ada")
	m.Hello("ada")
	m.Hello("bob")

	Verify(m.Hello("ada")).Times(2)
	Verify(m.Hello("bob")).Once()
	Verify(m.Hello("nobody")).Never()
}

func TestSpyDefault(t *testing.T) {
	s := Spy[Greeter](realGreeter{})
	if got := s.Hello("ada"); got != "hello, ada" {
		t.Fatalf("got %q, want %q", got, "hello, ada")
	}
}

func TestSpyOverride(t *testing.T) {
	s := Spy[Greeter](realGreeter{})
	When(s.Hello("bob")).ThenReturn("hi bob")

	if got := s.Hello("bob"); got != "hi bob" {
		t.Fatalf("got %q, want %q", got, "hi bob")
	}
	if got := s.Hello("ada"); got != "hello, ada" {
		t.Fatalf("got %q, want %q", got, "hello, ada")
	}
}

func TestStubWhen3(t *testing.T) {
	m := Mock[Greeter]()
	When3(m.Triple("ada")).ThenReturn("hi ada", 1, nil)

	got, n, err := m.Triple("ada")
	if got != "hi ada" || n != 1 || err != nil {
		t.Fatalf("got (%q, %d, %v)", got, n, err)
	}
}

func TestStubWhen4(t *testing.T) {
	m := Mock[Greeter]()
	When4(m.Quad("ada")).ThenReturn("hi ada", 1, true, nil)

	got, n, ok, err := m.Quad("ada")
	if got != "hi ada" || n != 1 || !ok || err != nil {
		t.Fatalf("got (%q, %d, %v, %v)", got, n, ok, err)
	}
}

func TestVerify2(t *testing.T) {
	m := Mock[Greeter]()
	m.Greet("ada")
	m.Greet("ada")
	Verify2(m.Greet("ada")).Times(2)
}

func TestVerify3(t *testing.T) {
	m := Mock[Greeter]()
	m.Triple("ada")
	Verify3(m.Triple("ada")).Once()
}

func TestVerify4(t *testing.T) {
	m := Mock[Greeter]()
	m.Quad("ada")
	m.Quad("ada")
	Verify4(m.Quad("ada")).Times(2)
}

func TestStubReturnError(t *testing.T) {
	m := Mock[Greeter]()
	When2(m.Greet("ada")).ThenReturn("", errors.New("boom"))

	_, err := m.Greet("ada")
	if err == nil || err.Error() != "boom" {
		t.Fatalf("got err %v, want boom", err)
	}
}

func TestStubThenPanic(t *testing.T) {
	m := Mock[Greeter]()
	When(m.Hello("ada")).ThenPanic("boom")

	defer func() {
		if r := recover(); r != "boom" {
			t.Fatalf("got panic %v, want boom", r)
		}
	}()
	m.Hello("ada")
}

func TestVerifyAtLeastAtMost(t *testing.T) {
	m := Mock[Greeter]()
	m.Hello("ada")
	m.Hello("ada")
	m.Hello("ada")

	Verify(m.Hello("ada")).AtLeast(2)
	Verify(m.Hello("ada")).AtMost(3)
	Verify(m.Hello("ada")).Between(2, 3)
	Verify(m.Hello("bob")).AtMost(0)
}

func TestInOrder(t *testing.T) {
	m := Mock[Greeter]()
	m.Hello("ada")
	m.Hello("bob")
	m.Hello("ada")

	io := NewInOrder()
	io.Verify(m.Hello("ada")).Once()
	io.Verify(m.Hello("bob")).Once()
	io.Verify(m.Hello("ada")).Once()
}

func TestStubSequence(t *testing.T) {
	m := Mock[Greeter]()
	// Single-result method: ThenReturn values form a sequence, and the last
	// value repeats once the sequence is exhausted.
	When(m.Hello("ada")).ThenReturn("first", "second", "third")

	for i, want := range []string{"first", "second", "third", "third"} {
		if got := m.Hello("ada"); got != want {
			t.Fatalf("call %d = %q, want %q", i, got, want)
		}
	}
}

func TestStubSequenceMulti(t *testing.T) {
	m := Mock[Greeter]()
	// Multi-result method: each ThenReturn is one group; the chain is the
	// sequence.
	When2(m.Greet("ada")).ThenReturn("a", nil).ThenReturn("b", errors.New("boom"))

	got, err := m.Greet("ada")
	if got != "a" || err != nil {
		t.Fatalf("call 1 = (%q, %v), want (a, nil)", got, err)
	}
	got, err = m.Greet("ada")
	if got != "b" || err == nil || err.Error() != "boom" {
		t.Fatalf("call 2 = (%q, %v), want (b, boom)", got, err)
	}
}

func TestVerifyNoInteractions(t *testing.T) {
	m := Mock[Greeter]()
	VerifyNoInteractions(m)
}

func TestVerifyNoMoreInteractions(t *testing.T) {
	m := Mock[Greeter]()
	m.Hello("ada")
	Verify(m.Hello("ada")).Once()
	VerifyNoMoreInteractions(m)
}

func TestVerifyNoMoreInteractionsFails(t *testing.T) {
	m := Mock[Greeter]()
	m.Hello("ada")
	m.Hello("bob") // never verified
	Verify(m.Hello("ada")).Once()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for the unverified call")
		}
	}()
	VerifyNoMoreInteractions(m)
}

func TestVerifyTimeout(t *testing.T) {
	m := Mock[Greeter]()
	go func() {
		time.Sleep(20 * time.Millisecond)
		m.Hello("ada")
	}()
	Verify(m.Hello("ada")).Timeout(time.Second).Once()
}

// fakeTB implements just enough of testing.TB to capture Errorf.
type fakeTB struct {
	testing.TB
	errs []string
}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.errs = append(f.errs, fmt.Sprintf(format, args...))
}

func TestVerifyWithTReportsInsteadOfPanic(t *testing.T) {
	m := Mock[Greeter]()
	m.Hello("ada")

	tb := &fakeTB{}
	Verify(m.Hello("ada")).T(tb).Times(2) // fails, reported via Errorf
	Verify(m.Hello("bob")).T(tb).Never()  // passes

	if len(tb.errs) != 1 {
		t.Fatalf("got %d errors, want 1: %v", len(tb.errs), tb.errs)
	}
}

func TestVerifyMultipleFailuresAllReported(t *testing.T) {
	m := Mock[Greeter]()
	m.Hello("ada")

	tb := &fakeTB{}
	Verify(m.Hello("ada")).T(tb).Times(2) // fail 1
	Verify(m.Hello("bob")).T(tb).Once()   // fail 2

	if len(tb.errs) != 2 {
		t.Fatalf("got %d errors, want 2: %v", len(tb.errs), tb.errs)
	}
}

func TestStubAnyMatcher(t *testing.T) {
	m := Mock[Greeter]()
	When(m.Hello(Any[string]())).ThenReturn("hi any")

	if got := m.Hello("ada"); got != "hi any" {
		t.Fatalf("got %q, want hi any", got)
	}
	if got := m.Hello("bob"); got != "hi any" {
		t.Fatalf("got %q, want hi any", got)
	}
}

func TestStubEqMatcher(t *testing.T) {
	m := Mock[Greeter]()
	When(m.Hello(Eq("ada"))).ThenReturn("hi ada")

	if got := m.Hello("ada"); got != "hi ada" {
		t.Fatalf("got %q, want hi ada", got)
	}
	if got := m.Hello("bob"); got != "" {
		t.Fatalf("unstubbed call got %q, want zero", got)
	}
}

func TestStubMatchMatcher(t *testing.T) {
	m := Mock[Greeter]()
	When(m.Hello(Match(func(s string) bool { return len(s) > 3 }))).ThenReturn("long")

	if got := m.Hello("abcd"); got != "long" {
		t.Fatalf("got %q, want long", got)
	}
	if got := m.Hello("ab"); got != "" {
		t.Fatalf("got %q, want zero", got)
	}
}

func TestStubContainsMatcher(t *testing.T) {
	m := Mock[Greeter]()
	When(m.Hello(Contains("ell"))).ThenReturn("has ell")

	if got := m.Hello("hello"); got != "has ell" {
		t.Fatalf("got %q, want has ell", got)
	}
	if got := m.Hello("world"); got != "" {
		t.Fatalf("got %q, want zero", got)
	}
}

func TestVerifyWithMatcher(t *testing.T) {
	m := Mock[Greeter]()
	m.Hello("ada")
	m.Hello("bob")

	Verify(m.Hello(Any[string]())).Times(2)
}
