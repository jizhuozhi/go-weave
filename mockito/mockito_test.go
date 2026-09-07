package mockito

import (
	"errors"
	"testing"
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
