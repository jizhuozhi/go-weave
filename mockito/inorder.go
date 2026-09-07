package mockito

import "fmt"

// InOrder verifies the relative order of calls. It keeps a cursor; each Verify
// finds the next match from the cursor and advances past it, asserting that the
// previous Verify's call happened before the next.
type InOrder struct {
	s   *state
	pos int
}

// NewInOrder returns a call-order verifier, bound to a mock on first Verify.
func NewInOrder() *InOrder { return &InOrder{} }

func (io *InOrder) Verify(_ any) *InOrderVerifier { return io.verify() }
func (io *InOrder) Verify2(_ any, _ any) *InOrderVerifier {
	return io.verify()
}
func (io *InOrder) Verify3(_ any, _ any, _ any) *InOrderVerifier {
	return io.verify()
}
func (io *InOrder) Verify4(_ any, _ any, _ any, _ any) *InOrderVerifier {
	return io.verify()
}

func (io *InOrder) verify() *InOrderVerifier {
	p := popLast()
	if p == nil {
		panic("mockito: InOrder.Verify must wrap a mock method call")
	}
	p.state.rollback()
	if io.s == nil {
		io.s = p.state
	}
	return &InOrderVerifier{io: io, codePtr: p.codePtr, args: p.args}
}

// InOrderVerifier is the chainable result of InOrder.Verify.
type InOrderVerifier struct {
	io      *InOrder
	codePtr uintptr
	args    []any
}

// Times asserts exactly n matching calls from the cursor, then advances past them.
func (iv *InOrderVerifier) Times(n int) {
	io := iv.io
	io.s.mu.Lock()
	defer io.s.mu.Unlock()
	count, last := 0, -1
	for i := io.pos; i < len(io.s.calls); i++ {
		c := io.s.calls[i]
		if c.CodePtr == iv.codePtr && argsMatch(iv.args, c.Args) {
			count++
			last = i
			if count == n {
				break
			}
		}
	}
	if count != n {
		panic(fmt.Sprintf("mockito: in-order %d calls, want %d", count, n))
	}
	if last >= 0 {
		io.pos = last + 1
	}
}

// Once asserts exactly one matching call from the cursor.
func (iv *InOrderVerifier) Once() { iv.Times(1) }
