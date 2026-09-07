package mockito

import "fmt"

// InOrder 验证调用的相对顺序。它维护一个游标，每次 Verify 从游标起找下一次
// 匹配，并把游标推进到其后——从而断言"前一次 Verify 的调用发生在后一次之前"。
type InOrder struct {
	s   *state
	pos int
}

// NewInOrder 返回一个调用顺序验证器。首次 Verify 时通过记录式绑定到 mock。
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

// InOrderVerifier 是 InOrder.Verify 的链式调用结果。
type InOrderVerifier struct {
	io      *InOrder
	codePtr uintptr
	args    []any
}

// Times 断言从游标起恰好有 n 次匹配调用，并把游标推进到最后一次之后。
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

// Once 断言从游标起恰好一次匹配调用。
func (iv *InOrderVerifier) Once() { iv.Times(1) }
