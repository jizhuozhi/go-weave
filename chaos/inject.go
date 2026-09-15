package chaos

import (
	"context"
	"reflect"
	"sync/atomic"
	"time"

	weave "github.com/jizhuozhi/go-weave"
)

var (
	ctxType = reflect.TypeOf((*context.Context)(nil)).Elem()
	errType = reflect.TypeOf((*error)(nil)).Elem()
)

// compiled is one rule ready to apply, together with the state that changes
// while it runs. It is shared by every interface the rule was resolved
// against, which is what makes MaxCount a property of the rule rather than of
// any one proxy.
//
// The action list is flattened here: delays stack into one sleep, and the
// action that decides the call — check has seen to it that there is at most
// one, and that it is last — becomes err or panicV.
type compiled struct {
	// budget counts the faults this rule has injected. It comes first so that
	// it is aligned for an atomic update.
	budget uint64

	rate      float64
	latency   time.Duration
	err       error
	panicV    any
	hasPanic  bool
	maxCount  int
	hasWindow bool
	from      time.Time
	until     time.Time
}

// bound is one method's resolved fault, held in a table indexed by interface
// method index so that the hot path is an array index rather than a string
// lookup.
type bound struct {
	// seq is the sampling counter. It comes first so that it sits at the
	// start of the table's backing array and is aligned for an atomic update.
	seq uint64

	// rule is nil when no rule owns the method — the common case, and the one
	// that has to stay cheap.
	rule *compiled

	// errOut is the index of the method's error result, or -1 when it has
	// none and an error cannot be injected.
	errOut int
}

// binding is a plan resolved against one interface.
type binding struct {
	plan  *plan
	table []bound
}

// binder is one proxy's view of an injector: it holds the plan resolved for
// that proxy's interface and rebuilds the table when the plan is replaced.
type binder struct {
	in        *Injector
	ifaceName string
	methods   []*weave.Method
	cur       atomic.Value // *binding
}

// resolve returns the table for the current plan, rebuilding it when the plan
// has been replaced since the last call. Rebuilding is idempotent, so two
// goroutines racing to do it is harmless.
func (b *binder) resolve() *binding {
	p := b.in.current()
	if bd, _ := b.cur.Load().(*binding); bd != nil && bd.plan == p {
		return bd
	}
	table := make([]bound, len(b.methods))
	for i, m := range b.methods {
		c := p.match(b.ifaceName, m.Name)
		if c == nil {
			continue
		}
		table[i] = bound{rule: c, errOut: errorResult(m.Type)}
	}
	bd := &binding{plan: p, table: table}
	b.cur.Store(bd)
	return bd
}

// intercept is the single interceptor every proxy built by an injector runs.
//
// The order of the guards is the order of their cost: owning the call is free,
// the window costs a clock read only when one is set, the budget an atomic
// load only when a limit is set, and the sample an atomic add always. A rule
// that is out of its window or past its budget still owns the call — a less
// specific rule does not step in — the call is simply not faulted.
func (b *binder) intercept(c *weave.Invocation) []reflect.Value {
	// Method.Index is the itab slot number and the table is built from the
	// proxy's own method list, so the index is always in range.
	bd := b.resolve()
	e := &bd.table[c.Method.Index]
	r := e.rule
	if r == nil {
		return c.Proceed()
	}
	if r.hasWindow && !r.active() {
		return c.Proceed()
	}
	if r.maxCount > 0 && atomic.LoadUint64(&r.budget) >= uint64(r.maxCount) {
		return c.Proceed()
	}
	if sample(atomic.AddUint64(&e.seq, 1)) >= r.rate {
		return c.Proceed()
	}
	if r.maxCount > 0 {
		atomic.AddUint64(&r.budget, 1)
	}

	// The actions, flattened: the delay, then the one that decides the call.
	if r.latency > 0 {
		wait(c, r.latency)
	}
	if r.hasPanic {
		panic(r.panicV)
	}
	if r.err != nil && e.errOut >= 0 {
		out := make([]reflect.Value, c.Method.NumOut())
		for k := range out {
			out[k] = reflect.Zero(c.Method.Type.Out(k))
		}
		out[e.errOut] = reflect.ValueOf(r.err)
		return out
	}
	return c.Proceed()
}

// active reports whether the rule's window, if any, currently contains the
// clock.
func (r *compiled) active() bool {
	now := time.Now()
	if !r.from.IsZero() && now.Before(r.from) {
		return false
	}
	if !r.until.IsZero() && now.After(r.until) {
		return false
	}
	return true
}

// sample maps the k'th matching call to a value in [0, 1).
//
// The mix is splitmix64. Sampling from the call's ordinal rather than from an
// RNG keeps the hot path free of shared mutable state — no lock, no atomic on
// a global counter — and makes a run reproducible: the same call sequence
// always faults the same calls.
func sample(n uint64) float64 {
	x := n * 0x9E3779B97F4A7C15
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return float64(x>>11) * (1.0 / (1 << 53))
}

// wait sleeps for d, or until the call's context is done.
//
// A delay that ignored cancellation would hold a caller past its own deadline,
// which is the behaviour the experiment exists to surface rather than one it
// should introduce. When the context ends first the delay stops and the call
// proceeds, so the target sees the cancelled context and fails the way it
// normally would.
func wait(c *weave.Invocation, d time.Duration) {
	ctx, ok := callContext(c)
	if !ok {
		time.Sleep(d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// callContext returns the call's context.Context argument, if it has one.
//
// Only the argument types are inspected before anything is materialised, so a
// method with no context pays nothing for this: reading an argument would pull
// the whole call off the register fast path.
func callContext(c *weave.Invocation) (context.Context, bool) {
	ft := c.Method.Type
	for i := 0; i < ft.NumIn(); i++ {
		if !ft.In(i).Implements(ctxType) {
			continue
		}
		if ctx, ok := c.Arg(i).Interface().(context.Context); ok && ctx != nil {
			return ctx, true
		}
	}
	return nil, false
}

// errorResult returns the index of the method's error result, or -1.
//
// Only the error interface itself is accepted. Go puts the error last by
// convention, so the search runs backwards.
func errorResult(ft reflect.Type) int {
	for i := ft.NumOut() - 1; i >= 0; i-- {
		if ft.Out(i) == errType {
			return i
		}
	}
	return -1
}
