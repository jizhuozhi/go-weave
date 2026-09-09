package mockito

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/jizhuozhi/go-weave/internal/gls"
)

type Matcher interface {
	Matches(x any) bool
	String() string
}

var (
	matcherMu sync.Mutex
	matchers  = map[uintptr][]Matcher{}
)

func pushMatcher(m Matcher) {
	matcherMu.Lock()
	k := gls.Key()
	matchers[k] = append(matchers[k], m)
	matcherMu.Unlock()
}

func takeMatchers() []Matcher {
	matcherMu.Lock()
	k := gls.Key()
	ms := matchers[k]
	delete(matchers, k)
	matcherMu.Unlock()
	return ms
}

func Any[T any]() T {
	var zero T
	pushMatcher(anyMatcher{})
	return zero
}

func Eq[T any](v T) T {
	pushMatcher(eqMatcher{v: v})
	return v
}

func NotNil[T any]() T {
	var zero T
	pushMatcher(notNilMatcher{})
	return zero
}

func Match[T any](pred func(T) bool) T {
	var zero T
	pushMatcher(predMatcher{name: "match", f: func(x any) bool { return pred(x.(T)) }})
	return zero
}

func Contains(sub string) string {
	pushMatcher(stringMatcher{name: fmt.Sprintf("contains(%q)", sub), f: func(s string) bool { return strings.Contains(s, sub) }})
	return ""
}

func HasPrefix(prefix string) string {
	pushMatcher(stringMatcher{name: fmt.Sprintf("hasPrefix(%q)", prefix), f: func(s string) bool { return strings.HasPrefix(s, prefix) }})
	return ""
}

func HasSuffix(suffix string) string {
	pushMatcher(stringMatcher{name: fmt.Sprintf("hasSuffix(%q)", suffix), f: func(s string) bool { return strings.HasSuffix(s, suffix) }})
	return ""
}

type anyMatcher struct{}

func (anyMatcher) Matches(any) bool { return true }
func (anyMatcher) String() string   { return "any" }

type eqMatcher struct{ v any }

func (m eqMatcher) Matches(x any) bool { return reflect.DeepEqual(m.v, x) }
func (m eqMatcher) String() string     { return fmt.Sprintf("eq(%v)", m.v) }

type notNilMatcher struct{}

func (notNilMatcher) Matches(x any) bool {
	// x != nil is not enough: a nil pointer boxed into an interface is a
	// non-nil interface value. Reflect through to the underlying nilness.
	if x == nil {
		return false
	}
	v := reflect.ValueOf(x)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Chan, reflect.Func:
		return !v.IsNil()
	default:
		return true
	}
}
func (notNilMatcher) String() string { return "not nil" }

type predMatcher struct {
	name string
	f    func(any) bool
}

func (m predMatcher) Matches(x any) bool { return m.f(x) }
func (m predMatcher) String() string     { return m.name }

type stringMatcher struct {
	name string
	f    func(string) bool
}

func (m stringMatcher) Matches(x any) bool {
	s, ok := x.(string)
	return ok && m.f(s)
}
func (m stringMatcher) String() string { return m.name }
