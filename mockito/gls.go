package mockito

import (
	"reflect"
	"sync"

	"github.com/jizhuozhi/go-weave/internal/gls"
)

type pendingCall struct {
	state    *state
	codePtr  uintptr
	method   string // method name, for failure messages
	args     []any
	numOut   int       // number of results the method returns
	matchers []Matcher // per-argument matchers, nil entry = exact match
}

var (
	glsMu sync.Mutex
	lasts = map[uintptr]*pendingCall{}
)

func recordLast(s *state, codePtr uintptr, method string, args []reflect.Value, numOut int, matchers []Matcher) {
	glsMu.Lock()
	lasts[gls.Key()] = &pendingCall{state: s, codePtr: codePtr, method: method, args: snapshot(args), numOut: numOut, matchers: matchers}
	glsMu.Unlock()
}

func popLast() *pendingCall {
	k := gls.Key()
	glsMu.Lock()
	p := lasts[k]
	delete(lasts, k)
	glsMu.Unlock()
	return p
}
