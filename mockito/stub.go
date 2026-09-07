package mockito

// whenRecorded turns the current goroutine's last call into a stub.
func whenRecorded() *Stubber {
	p := popLast()
	if p == nil {
		panic("mockito: When must wrap a mock method call")
	}
	p.state.rollback() // the trigger call must not count toward Verify
	return &Stubber{s: p.state, codePtr: p.codePtr, args: p.args}
}

// When starts a recording-style stub: in When(m.Hello("ada")) the call
// m.Hello("ada") is real, so its arguments are compile-time checked, and the
// result count selects When / When2 / ... / When4 — fully type-safe.
func When[T any](_ T) *Stubber { return whenRecorded() }

// When2 is for methods returning two values (e.g. (T, error)).
func When2[T, E any](_ T, _ E) *Stubber { return whenRecorded() }

// When3 is for methods returning three values.
func When3[T1, T2, T3 any](_ T1, _ T2, _ T3) *Stubber { return whenRecorded() }

// When4 is for methods returning four values.
func When4[T1, T2, T3, T4 any](_ T1, _ T2, _ T3, _ T4) *Stubber { return whenRecorded() }

// Stubber is the chainable result of When.
type Stubber struct {
	s       *state
	codePtr uintptr
	args    []any
}

func (sb *Stubber) add(st *stub) {
	st.args = sb.args
	sb.s.addStub(sb.codePtr, st)
}

// ThenReturn declares the return values for a matching call. nil maps to the
// zero value of the corresponding result type (e.g. a nil error).
func (sb *Stubber) ThenReturn(results ...any) { sb.add(&stub{rets: results}) }

// ThenAnswer computes results dynamically from the call arguments.
func (sb *Stubber) ThenAnswer(f func([]any) []any) { sb.add(&stub{answer: f}) }

// ThenPanic makes a matching call panic(v) — Go's notion of "throwing".
func (sb *Stubber) ThenPanic(v any) { sb.add(&stub{panics: true, panicV: v}) }
