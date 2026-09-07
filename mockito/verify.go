package mockito

import "fmt"

// verifyRecorded 把当前 goroutine 的最后一次调用回收成一次验证。
func verifyRecorded() *Verifier {
	p := popLast()
	if p == nil {
		panic("mockito: Verify must wrap a mock method call")
	}
	p.state.rollback() // 触发调用不计入验证统计
	return &Verifier{s: p.state, codePtr: p.codePtr, args: p.args}
}

// Verify 开始一次记录式验证：Verify(m.Hello("ada")) 里 m.Hello("ada") 是真实
// 调用，参数受编译期类型检查，返回值数量由 Verify/Verify2/.../Verify4 区分。
func Verify[T any](_ T) *Verifier { return verifyRecorded() }

// Verify2 用于返回两个值的方法。
func Verify2[T, E any](_ T, _ E) *Verifier { return verifyRecorded() }

// Verify3 用于返回三个值的方法。
func Verify3[T1, T2, T3 any](_ T1, _ T2, _ T3) *Verifier { return verifyRecorded() }

// Verify4 用于返回四个值的方法。
func Verify4[T1, T2, T3, T4 any](_ T1, _ T2, _ T3, _ T4) *Verifier { return verifyRecorded() }

// Verifier 是 Verify 的链式调用结果。
type Verifier struct {
	s       *state
	codePtr uintptr
	args    []any
}

// Count 返回匹配的调用次数。
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

// Times 断言匹配调用恰好发生 n 次，否则 panic。
func (v *Verifier) Times(n int) {
	if got := v.Count(); got != n {
		panic(fmt.Sprintf("mockito: %d calls, want %d", got, n))
	}
}

// AtLeast 断言匹配调用至少发生 n 次。
func (v *Verifier) AtLeast(n int) {
	if got := v.Count(); got < n {
		panic(fmt.Sprintf("mockito: %d calls, want at least %d", got, n))
	}
}

// AtMost 断言匹配调用至多发生 n 次。
func (v *Verifier) AtMost(n int) {
	if got := v.Count(); got > n {
		panic(fmt.Sprintf("mockito: %d calls, want at most %d", got, n))
	}
}

// Between 断言匹配调用次数落在 [a, b] 区间内。
func (v *Verifier) Between(a, b int) {
	if got := v.Count(); got < a || got > b {
		panic(fmt.Sprintf("mockito: %d calls, want between %d and %d", got, a, b))
	}
}

// Never 断言从未被匹配调用。
func (v *Verifier) Never() { v.Times(0) }

// Once 断言恰好被匹配调用一次。
func (v *Verifier) Once() { v.Times(1) }
