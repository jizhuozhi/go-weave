// Package mockito is a Mockito-style mock / spy framework built on go-weave's
// runtime dynamic proxies.
//
// It targets three things gomock does badly: spies (partial mocks), mocking
// third-party / source-less interfaces, and deciding behavior at runtime. A
// mock is go-weave's nil-target special case.
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

// state is a mock's internal state: stub rules and the call log. Stubs are
// keyed by a method's code pointer (c.Method.CodePtr()), not its name.
type state struct {
	mu    sync.Mutex
	spy   bool
	stubs map[uintptr][]*stub
	calls []Call
}

// stub is one behavior rule: when the method is called with a given argument
// set, return a result.
type stub struct {
	args   []any // expected arguments, matched exactly
	rets   []any
	answer func([]any) []any // ThenAnswer closure: results computed from the call args
	panics bool              // ThenPanic: panic(panicV) on match
	panicV any
}

// Call records one method invocation. Method is for display, CodePtr for matching.
type Call struct {
	Method  string
	CodePtr uintptr
	Args    []any
}

// Mock returns a mock of T: every method returns zero values and records calls.
func Mock[T any]() T {
	s := &state{stubs: map[uintptr][]*stub{}}
	var zero T
	return weave.New[T](zero, s.intercept)
}

// Spy returns a spy of T: methods run the real target unless overridden.
func Spy[T any](target T) T {
	s := &state{spy: true, stubs: map[uintptr][]*stub{}}
	return weave.New[T](target, s.intercept)
}

// intercept is the core interceptor: record the call, match a stub, else fall back.
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

// findStub returns the last matching stub (later stubs win, like Mockito).
func (s *state) findStub(codePtr uintptr, args []reflect.Value) *stub {
	stubs := s.stubs[codePtr]
	for i := len(stubs) - 1; i >= 0; i-- {
		if stubs[i].match(args) {
			return stubs[i]
		}
	}
	return nil
}

// rollback removes the last recorded call — the recording-style trigger call,
// which must not count toward verification.
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

// values converts stub results to reflect.Value; nil maps to the zero value of
// the corresponding result type.
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

// argsMatch reports whether expected arguments match actual arguments exactly.
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
