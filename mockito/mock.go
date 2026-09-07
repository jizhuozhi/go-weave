// Package mockito 是构建在 go-weave 运行时动态代理之上的 mock / spy 框架。
//
// 它回答 gomock 答不好的三件事：spy（部分 mock）、mock 第三方/无源码接口、
// 以及运行时动态决定行为。mock 是 go-weave 的 nil-target 特例。
//
//	m := mockito.Mock[Greeter]()
//	mockito.When(m.Hello("ada")).ThenReturn("hi ada")
//	m.Hello("ada")                    // "hi ada"
//	mockito.Verify(m.Hello("ada")).Once()
package mockito

import (
	"reflect"
	"sync"

	weave "github.com/jizhuozhi/go-weave"
)

// state 是 mock 的内部状态：stub 规则 + 调用记录。stub 按方法 codePtr 索引
// （c.Method.CodePtr()），而非方法名字符串——更稳、更快。
type state struct {
	mu    sync.Mutex
	spy   bool
	stubs map[uintptr][]*stub
	calls []Call
}

// stub 一条行为规则：当方法被某组参数调用时，返回指定结果。
type stub struct {
	args   []any // 期望参数，精确匹配
	rets   []any
	answer func([]any) []any // ThenAnswer 闭包，按调用参数动态生成结果
	panics bool              // ThenPanic：匹配时 panic(panicV)
	panicV any
}

// Call 记录一次方法调用。Method 供展示，CodePtr 供匹配。
type Call struct {
	Method  string
	CodePtr uintptr
	Args    []any
}

// Mock 返回 T 的一个 mock：所有方法默认返回零值，并记录每次调用。
func Mock[T any]() T {
	s := &state{stubs: map[uintptr][]*stub{}}
	var zero T
	return weave.New[T](zero, s.intercept)
}

// Spy 返回 T 的一个 spy：方法默认走真实 target，除非被 stub 覆盖。
func Spy[T any](target T) T {
	s := &state{spy: true, stubs: map[uintptr][]*stub{}}
	return weave.New[T](target, s.intercept)
}

// intercept 是核心拦截器：记录调用 → 匹配 stub → 回落到默认行为。
func (s *state) intercept(c *weave.Invocation) []reflect.Value {
	s.mu.Lock()
	defer s.mu.Unlock()

	args := c.Args()
	codePtr := c.Method.CodePtr()
	s.calls = append(s.calls, Call{
		Method:  c.Method.Name,
		CodePtr: codePtr,
		Args:    snapshot(args),
	})
	recordLast(s, codePtr, args)

	if st := s.findStub(codePtr, args); st != nil {
		if st.panics {
			panic(st.panicV)
		}
		return st.values(c.Method.Type, args)
	}
	if s.spy {
		return c.Proceed()
	}
	return zeroResults(c)
}

// findStub 从后往前找第一个匹配的 stub（后定义者优先，同 Mockito）。
func (s *state) findStub(codePtr uintptr, args []reflect.Value) *stub {
	stubs := s.stubs[codePtr]
	for i := len(stubs) - 1; i >= 0; i-- {
		if stubs[i].match(args) {
			return stubs[i]
		}
	}
	return nil
}

// rollback 移除最后一次调用记录——它是记录式 stub/verify 的触发调用，不计入统计。
func (s *state) rollback() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.calls); n > 0 {
		s.calls = s.calls[:n-1]
	}
}

func (s *state) addStub(codePtr uintptr, st *stub) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stubs[codePtr] = append(s.stubs[codePtr], st)
}

func (st *stub) match(args []reflect.Value) bool {
	return argsMatch(st.args, snapshot(args))
}

// values 把 stub 结果转成 reflect.Value；nil 用对应返回类型的零值。
func (st *stub) values(methodType reflect.Type, args []reflect.Value) []reflect.Value {
	rets := st.rets
	if st.answer != nil {
		rets = st.answer(snapshot(args))
	}
	out := make([]reflect.Value, len(rets))
	for i, r := range rets {
		if r == nil {
			out[i] = reflect.Zero(methodType.Out(i))
		} else {
			out[i] = reflect.ValueOf(r)
		}
	}
	return out
}

// argsMatch 判断期望参数与实参是否精确匹配。
func argsMatch(want, got []any) bool {
	if len(want) != len(got) {
		return false
	}
	for i, w := range want {
		if !reflect.DeepEqual(w, got[i]) {
			return false
		}
	}
	return true
}

func snapshot(args []reflect.Value) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = a.Interface()
	}
	return out
}

func zeroResults(c *weave.Invocation) []reflect.Value {
	out := make([]reflect.Value, c.Method.NumOut())
	for i := range out {
		out[i] = reflect.Zero(c.Method.Type.Out(i))
	}
	return out
}
