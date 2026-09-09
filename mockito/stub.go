package mockito

// whenRecorded turns the current goroutine's last call into a stub.
func whenRecorded() *Stubber {
	p := popLast()
	if p == nil {
		panic("mockito: When must wrap a mock method call")
	}
	p.state.rollback() // the trigger call must not count toward Verify
	return &Stubber{s: p.state, codePtr: p.codePtr, args: p.args, numOut: p.numOut, matchers: p.matchers}
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
	s        *state
	codePtr  uintptr
	args     []any
	matchers []Matcher
	numOut   int
	stub     *stub // the rule being built, so ThenReturn chains append to it
}

// stubOrCreate returns the in-progress stub, registering it on first use.
func (sb *Stubber) stubOrCreate() *stub {
	if sb.stub == nil {
		sb.stub = &stub{args: sb.args, matchers: sb.matchers}
		sb.s.addStub(sb.codePtr, sb.stub)
	}
	return sb.stub
}

// ThenReturn declares the return values for a matching call and chains, so a
// later ThenReturn appends to the same rule. For a single-result method the
// values form a sequence (1st call -> v1, 2nd -> v2, ...); for a multi-result
// method each ThenReturn is one result group and the chain is the sequence. nil
// maps to the zero value of the corresponding result type.
func (sb *Stubber) ThenReturn(results ...any) *Stubber {
	st := sb.stubOrCreate()
	if sb.numOut == 1 {
		for _, r := range results {
			st.retSets = append(st.retSets, []any{r})
		}
	} else {
		st.retSets = append(st.retSets, results)
	}
	return sb
}

// ThenAnswer computes results dynamically from the call arguments.
func (sb *Stubber) ThenAnswer(f func([]any) []any) *Stubber {
	sb.stubOrCreate().answer = f
	return sb
}

// ThenPanic makes a matching call panic(v) — Go's notion of "throwing".
func (sb *Stubber) ThenPanic(v any) *Stubber {
	st := sb.stubOrCreate()
	st.panics = true
	st.panicV = v
	return sb
}
