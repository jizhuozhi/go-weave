package mockito

import "fmt"

// verifyRecorded turns the current goroutine's last call into a verification.
func verifyRecorded() *Verifier {
	p := popLast()
	if p == nil {
		panic("mockito: Verify must wrap a mock method call")
	}
	p.state.rollback() // the trigger call must not count toward verification
	return &Verifier{s: p.state, codePtr: p.codePtr, args: p.args}
}

// Verify starts a recording-style verification: in Verify(m.Hello("ada")) the
// call is real, so its arguments are compile-time checked, and the result count
// selects Verify / Verify2 / ... / Verify4.
func Verify[T any](_ T) *Verifier { return verifyRecorded() }

// Verify2 is for methods returning two values.
func Verify2[T, E any](_ T, _ E) *Verifier { return verifyRecorded() }

// Verify3 is for methods returning three values.
func Verify3[T1, T2, T3 any](_ T1, _ T2, _ T3) *Verifier { return verifyRecorded() }

// Verify4 is for methods returning four values.
func Verify4[T1, T2, T3, T4 any](_ T1, _ T2, _ T3, _ T4) *Verifier { return verifyRecorded() }

// Verifier is the chainable result of Verify.
type Verifier struct {
	s       *state
	codePtr uintptr
	args    []any
}

// Count returns the number of matching calls.
func (v *Verifier) Count() int {
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	n := 0
	for _, c := range v.s.calls {
		if c.CodePtr == v.codePtr && argsMatch(v.args, c.Args) {
			n++
		}
	}
	return n
}

// Times asserts exactly n matching calls.
func (v *Verifier) Times(n int) {
	if got := v.Count(); got != n {
		panic(fmt.Sprintf("mockito: %d calls, want %d", got, n))
	}
}

// AtLeast asserts at least n matching calls.
func (v *Verifier) AtLeast(n int) {
	if got := v.Count(); got < n {
		panic(fmt.Sprintf("mockito: %d calls, want at least %d", got, n))
	}
}

// AtMost asserts at most n matching calls.
func (v *Verifier) AtMost(n int) {
	if got := v.Count(); got > n {
		panic(fmt.Sprintf("mockito: %d calls, want at most %d", got, n))
	}
}

// Between asserts the matching call count falls in [a, b].
func (v *Verifier) Between(a, b int) {
	if got := v.Count(); got < a || got > b {
		panic(fmt.Sprintf("mockito: %d calls, want between %d and %d", got, a, b))
	}
}

// Never asserts zero matching calls.
func (v *Verifier) Never() { v.Times(0) }

// Once asserts exactly one matching call.
func (v *Verifier) Once() { v.Times(1) }
