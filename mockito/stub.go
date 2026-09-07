package mockito

// whenRecorded 把当前 goroutine 的最后一次调用回收成一条 stub。
func whenRecorded() *Stubber {
	p := popLast()
	if p == nil {
		panic("mockito: When must wrap a mock method call")
	}
	p.state.rollback() // 触发调用不计入 Verify
	return &Stubber{s: p.state, codePtr: p.codePtr, args: p.args}
}

// When 开始一条记录式 stub：When(m.Hello("ada")) 里 m.Hello("ada") 是真实调用，
// 参数受编译期类型检查，返回值数量由 When/When2/.../When4 区分——全程强类型。
func When[T any](_ T) *Stubber { return whenRecorded() }

// When2 用于返回两个值的方法（如 (T, error)）。
func When2[T, E any](_ T, _ E) *Stubber { return whenRecorded() }

// When3 用于返回三个值的方法。
func When3[T1, T2, T3 any](_ T1, _ T2, _ T3) *Stubber { return whenRecorded() }

// When4 用于返回四个值的方法。
func When4[T1, T2, T3, T4 any](_ T1, _ T2, _ T3, _ T4) *Stubber { return whenRecorded() }

// Stubber 是 When 的链式调用结果。
type Stubber struct {
	s       *state
	codePtr uintptr
	args    []any
}

func (sb *Stubber) add(st *stub) {
	st.args = sb.args
	sb.s.addStub(sb.codePtr, st)
}

// ThenReturn 声明匹配调用的返回值。nil 会映射成对应返回类型的零值（如 nil error）。
// 返回 error 也用它：ThenReturn(零值..., err)。
func (sb *Stubber) ThenReturn(results ...any) { sb.add(&stub{rets: results}) }

// ThenAnswer 用一个闭包按调用参数动态生成返回值。闭包收到参数（[]any），
// 返回结果（[]any），nil 同样映射成对应返回类型的零值。
func (sb *Stubber) ThenAnswer(f func([]any) []any) { sb.add(&stub{answer: f}) }

// ThenPanic 让匹配调用 panic(v)。这是 Go 的"异常"；要返回 error 请用 ThenReturn。
func (sb *Stubber) ThenPanic(v any) { sb.add(&stub{panics: true, panicV: v}) }
