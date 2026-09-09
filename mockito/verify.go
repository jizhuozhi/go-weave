package mockito

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// verifyPollInterval is how often a timed verification re-checks the call log.
const verifyPollInterval = time.Millisecond

// fail reports a verification failure: through t if bound, else by panic.
// With t, the failure marks the test failed but lets it keep running, so
// several verifications can be reported in one run instead of the first one
// aborting the test.
func fail(t testing.TB, msg string) {
	if t != nil {
		t.Errorf("mockito: %s", msg)
		return
	}
	panic("mockito: " + msg)
}

// verifyRecorded turns the current goroutine's last call into a verification.
func verifyRecorded() *Verifier {
	p := popLast()
	if p == nil {
		panic("mockito: Verify must wrap a mock method call")
	}
	p.state.rollback() // the trigger call must not count toward verification
	return &Verifier{s: p.state, codePtr: p.codePtr, method: p.method, args: p.args, matchers: p.matchers}
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
	s        *state
	codePtr  uintptr
	method   string
	args     []any
	matchers []Matcher
	timeout  time.Duration
	t        testing.TB
}

// describe renders the expected call for a failure message, using a matcher's
// String() where one is bound.
func (v *Verifier) describe() string {
	parts := make([]string, len(v.args))
	for i, a := range v.args {
		if i < len(v.matchers) && v.matchers[i] != nil {
			parts[i] = v.matchers[i].String()
		} else {
			parts[i] = fmt.Sprintf("%v", a)
		}
	}
	return fmt.Sprintf("%s(%s)", v.method, strings.Join(parts, ", "))
}

// T binds the verifier to a testing.TB, so a failed assertion is reported via
// t.Errorf instead of panic.
func (v *Verifier) T(t testing.TB) *Verifier {
	v.t = t
	return v
}

// Timeout makes the following assertion wait up to d for the calls to happen,
// polling the call log. It targets calls made from other goroutines.
func (v *Verifier) Timeout(d time.Duration) *Verifier {
	v.timeout = d
	return v
}

// Count returns the number of matching calls, without marking them verified.
func (v *Verifier) Count() int {
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	return v.countLocked(false)
}

// countLocked counts matching calls; with mark it also records them as
// verified, which is what VerifyNoMoreInteractions keys off.
func (v *Verifier) countLocked(mark bool) int {
	n := 0
	for i, c := range v.s.calls {
		if c.CodePtr == v.codePtr && argsMatch(v.args, v.matchers, c.Args) {
			n++
			if mark {
				v.s.verified[i] = true
			}
		}
	}
	return n
}

// assert runs check against the count, polling first in Timeout mode.
func (v *Verifier) assert(check func(got int) (bool, string)) {
	if v.timeout > 0 && !v.wait(check) {
		return // timed out, already reported
	}
	v.s.mu.Lock()
	got := v.countLocked(true)
	v.s.mu.Unlock()
	if ok, msg := check(got); !ok {
		fail(v.t, msg)
	}
}

// wait polls until check passes; it reports and returns false on timeout.
func (v *Verifier) wait(check func(got int) (bool, string)) bool {
	deadline := time.Now().Add(v.timeout)
	for {
		got := v.Count()
		if ok, _ := check(got); ok {
			return true
		}
		if time.Now().After(deadline) {
			_, msg := check(got)
			fail(v.t, msg+" (timed out)")
			return false
		}
		time.Sleep(verifyPollInterval)
	}
}

// Times asserts exactly n matching calls.
func (v *Verifier) Times(n int) {
	v.assert(func(got int) (bool, string) {
		return got == n, fmt.Sprintf("%s: %d calls, want %d", v.describe(), got, n)
	})
}

// AtLeast asserts at least n matching calls.
func (v *Verifier) AtLeast(n int) {
	v.assert(func(got int) (bool, string) {
		return got >= n, fmt.Sprintf("%s: %d calls, want at least %d", v.describe(), got, n)
	})
}

// AtMost asserts at most n matching calls.
func (v *Verifier) AtMost(n int) {
	v.assert(func(got int) (bool, string) {
		return got <= n, fmt.Sprintf("%s: %d calls, want at most %d", v.describe(), got, n)
	})
}

// Between asserts the matching call count falls in [a, b].
func (v *Verifier) Between(a, b int) {
	v.assert(func(got int) (bool, string) {
		return got >= a && got <= b, fmt.Sprintf("%s: %d calls, want between %d and %d", v.describe(), got, a, b)
	})
}

// Never asserts zero matching calls.
func (v *Verifier) Never() { v.Times(0) }

// Once asserts exactly one matching call.
func (v *Verifier) Once() { v.Times(1) }

// VerifyNoInteractions asserts the mock was never called.
func VerifyNoInteractions(m any) { verifyNoInteractions(m, nil) }

// VerifyNoInteractionsT is VerifyNoInteractions reporting through t.
func VerifyNoInteractionsT(t testing.TB, m any) { verifyNoInteractions(m, t) }

func verifyNoInteractions(m any, t testing.TB) {
	s := stateOf(m)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) > 0 {
		fail(t, fmt.Sprintf("expected no interactions, got %d calls", len(s.calls)))
	}
}

// VerifyNoMoreInteractions asserts every call has been consumed by a Verify,
// so any unverified call — including an unexpected argument — fails.
func VerifyNoMoreInteractions(m any) { verifyNoMoreInteractions(m, nil) }

// VerifyNoMoreInteractionsT is VerifyNoMoreInteractions reporting through t.
func VerifyNoMoreInteractionsT(t testing.TB, m any) { verifyNoMoreInteractions(m, t) }

func verifyNoMoreInteractions(m any, t testing.TB) {
	s := stateOf(m)
	s.mu.Lock()
	defer s.mu.Unlock()
	var unexpected []string
	for i, c := range s.calls {
		if !s.verified[i] {
			unexpected = append(unexpected, fmt.Sprintf("%s(%v)", c.Method, c.Args))
		}
	}
	if len(unexpected) > 0 {
		fail(t, fmt.Sprintf("unexpected calls: %v", unexpected))
	}
}
