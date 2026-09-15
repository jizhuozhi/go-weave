// Package chaos injects faults into proxied interfaces at runtime.
//
// A fault is a delay, an error or a panic, applied to a fraction of the calls
// that match a rule. It is a go-weave interceptor chain of one link, so it
// works on any interface — third-party and source-less ones included — and
// needs no codegen.
//
//	inj := chaos.New(
//		chaos.Rule{Method: "GetUser", Rate: 1,
//			Actions: []chaos.Action{chaos.Delay(500 * time.Millisecond)}},
//		chaos.Rule{Interface: "UserRepo", Method: "Save", Rate: 0.1,
//			Actions: []chaos.Action{chaos.Fail(errInjected)}},
//	)
//	defer inj.Disable()
//
//	repo := chaos.Wrap[UserRepo](inj, realRepo)
//	svc := chaos.Wrap[UserService](inj, realSvc)
//
// # Taking over and injecting are separate
//
// Wrapping an interface installs a control point. What passes through it is
// decided separately, so an injector can be created with no rules at all and
// its rule set replaced at any time:
//
//	inj := chaos.New()                        // install the control point
//	repo := chaos.Wrap[UserRepo](inj, realRepo)
//
//	inj.Set(rules...)                         // whatever decides the rules
//	inj.Disable()                             // and whatever stops them
//
// A rule set installed with Set reaches every interface wrapped from the
// injector, and is replaced for all of them at once. The package owns the rule
// set and its lifecycle, not where the rules come from: fetching them, and
// decoding whatever the far side speaks, belongs to the control plane that
// sends them. Spec is the shape such a source decodes into, and Spec.Rule is
// the one place the mapping onto a Rule is defined.
//
// # Matching
//
// A rule's condition is an optional interface type name and an optional method
// name; either may be empty, which matches everything. Conditions are ordered
// by specificity, and a rule set has to be a strict partial order under that
// relation. Two rules claiming the same condition are rejected, and so are two
// rules that overlap without one being the more specific. What that buys is
// the invariant the package is built around: exactly one rule owns every
// call. Effects are not composed across rules — a rule that wants several
// carries them as several actions.
//
// # Effects
//
// A rule applies its actions one by one, in the order they are declared. An
// action is exactly one fault: a delay, an error, or a panic. An error or a
// panic decides the call, so it ends the list — nothing after it could run,
// and a rule that configures something there is rejected. Delays stack.
//
// # Cost
//
// The rules are compiled once per interface into a table indexed by interface
// method index, so a call that matches no rule costs an array index on top of
// the proxy's register fast path. A disabled injector matches nothing.
//
// Injecting a latency into a method that takes a context.Context makes the
// wait cancellable, so a caller with a deadline is not held past it: the delay
// ends, the call proceeds, and the target sees the cancelled context itself.
// Only the argument types are inspected to find the context, so methods
// without one stay on the fast path.
//
// # Sampling
//
// Which calls get the fault is deterministic: the k'th matching call is
// faulted when a hash of k falls below Rate. A given call sequence therefore
// always produces the same fault pattern, which makes an experiment
// reproducible without a seeded RNG, and needs no shared RNG state on the hot
// path.
package chaos

import (
	"fmt"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	weave "github.com/jizhuozhi/go-weave"
)

// Action is one fault. Exactly one of Latency, Err and Panic is set; a rule
// that wants several faults carries several actions.
type Action struct {
	// Latency is slept before whatever comes next: the following action, or
	// the call itself.
	Latency time.Duration

	// Err short-circuits the call: the target is not invoked and the method's
	// error result is set to Err. A method with no error result is unaffected
	// by it.
	Err error

	// Panic is panicked with instead of calling the target. The panic unwinds
	// through the caller exactly as one from the target would: weave recovers
	// nothing.
	Panic any
}

// Delay returns an action that sleeps for d before whatever comes next.
func Delay(d time.Duration) Action { return Action{Latency: d} }

// Fail returns an action that short-circuits the call with err.
func Fail(err error) Action { return Action{Err: err} }

// Panic returns an action that panics with v instead of calling the target.
func Panic(v any) Action { return Action{Panic: v} }

// Rule declares one injected fault.
//
// A rule's condition is Interface and Method. Its actions are applied one by
// one in order; the action that decides the call — Fail or Panic — must be the
// last one, because nothing after it could run. Rate must be in (0, 1]. New
// and Set panic on a rule set that breaks any of those, or whose conditions
// are not a strict partial order; see Validate.
type Rule struct {
	// Interface restricts the rule to interfaces whose type has this name,
	// matched against reflect.Type.Name. The empty string matches every
	// interface.
	Interface string

	// Method is the method name to match. The empty string matches every
	// method of the interfaces the condition selects.
	Method string

	// Rate is the fraction of matching calls that get the fault, in (0, 1].
	// A rate of 1 faults every matching call.
	Rate float64

	// Actions are the faults the rule applies, one by one in this order.
	// Delays stack; an error or a panic decides the call and ends the list.
	Actions []Action

	// MaxCount stops this rule from injecting after that many faults, counted
	// across every interface the rule was resolved against. Zero means no
	// limit. The count is part of the rule set: installing a new one with Set
	// starts it over.
	MaxCount int

	// From and Until bound when the rule is active. Either may be the zero
	// time, which leaves that side open. A call whose winning rule is outside
	// its window, or past its budget, is simply not faulted — the rule still
	// owns the call, and a less specific rule does not take over.
	From  time.Time
	Until time.Time
}

// Injector holds a rule set and applies it to every interface wrapped from it.
// All its methods are safe for concurrent use, including while calls are in
// flight.
type Injector struct {
	mu      sync.Mutex
	rules   []Rule
	enabled bool
	plan    atomic.Value // *plan
}

// New returns an injector applying rules, which may be none. An injector with
// no rules is a control point that is installed but injects nothing until a
// rule set arrives through Set.
//
// It panics if the rule set is unusable. Rules written as Go literals are a
// programmer's mistake when they are wrong and should fail at startup; rules
// arriving as data go through Spec.Rule and Validate first, which report
// instead.
func New(rules ...Rule) *Injector {
	if err := validateSet(rules); err != nil {
		panic(err)
	}
	in := &Injector{rules: cloneRules(rules)}
	in.enabled = true
	in.rebuild()
	return in
}

// Set replaces the rule set and enables injection.
//
// It panics if the rule set is unusable, and validates the whole set before
// touching anything, so a rejected call leaves the previous rule set running.
// A rule set built from Spec values and passed through Validate has already
// been checked and cannot panic here.
func (in *Injector) Set(rules ...Rule) {
	if err := validateSet(rules); err != nil {
		panic(err)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.rules = cloneRules(rules)
	in.enabled = true
	in.rebuild()
}

// Disable stops injection without discarding the rules. It is the kill switch:
// it takes effect on the next call, and a disabled injector adds nothing to
// the proxies it built.
func (in *Injector) Disable() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.enabled = false
	in.rebuild()
}

// Enable resumes injection with the rules last passed to Set.
func (in *Injector) Enable() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.enabled = true
	in.rebuild()
}

// Enabled reports whether injection is currently on.
func (in *Injector) Enabled() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.enabled
}

// Rules returns the rule set currently configured, whether or not injection is
// enabled. It is a copy; changing it changes nothing. This is what a control
// plane reads back to show what a process is actually running.
func (in *Injector) Rules() []Rule {
	in.mu.Lock()
	defer in.mu.Unlock()
	return cloneRules(in.rules)
}

// cloneRules copies a rule set deeply enough that a caller cannot reach into
// the injector's state through it. Copying the slice alone would leave the
// Actions slices shared, and a caller who mutated one would change what the
// next rebuild compiles — visible only after a later Disable or Enable.
func cloneRules(rules []Rule) []Rule {
	out := make([]Rule, len(rules))
	copy(out, rules)
	for i := range out {
		if out[i].Actions != nil {
			out[i].Actions = append([]Action(nil), out[i].Actions...)
		}
	}
	return out
}

// Validate reports why a rule set cannot be used, or nil.
//
// A rule set has to be a strict partial order under condition specificity. Two
// kinds of pair break that, and both are rejected here: two rules claiming the
// same condition, which would both own the same calls, and a rule scoped to an
// interface with a wildcard method next to a rule scoped to a method with a
// wildcard interface, which overlap without either being the more specific.
//
// New and Set panic with this error. A control plane assembling rule sets from
// outside the process should call Validate first and report, so that a bad
// push changes nothing instead of crashing the process it was meant to
// perturb.
func Validate(rules ...Rule) error {
	return validateSet(rules)
}

// rebuild publishes a fresh plan. Callers hold in.mu, and the rule set has
// already passed validateSet, so every condition key is unique and every
// action list is well formed. A disabled injector gets a plan with no rules,
// so its proxies match nothing and keep the fast path.
func (in *Injector) rebuild() {
	p := &plan{}
	if in.enabled {
		for i := range in.rules {
			r := &in.rules[i]
			c := &compiled{
				rate:      r.Rate,
				maxCount:  r.MaxCount,
				hasWindow: !r.From.IsZero() || !r.Until.IsZero(),
				from:      r.From,
				until:     r.Until,
			}
			for _, a := range r.Actions {
				switch {
				case a.Latency > 0:
					c.latency += a.Latency
				case a.Err != nil:
					c.err = a.Err
				case panics(a):
					c.hasPanic = true
					c.panicV = a.Panic
				}
			}
			if r.Interface == "" {
				if p.anyIface == nil {
					p.anyIface = map[string]*compiled{}
				}
				p.anyIface[r.Method] = c
				continue
			}
			if p.byIface == nil {
				p.byIface = map[string]map[string]*compiled{}
			}
			m := p.byIface[r.Interface]
			if m == nil {
				m = map[string]*compiled{}
				p.byIface[r.Interface] = m
			}
			m[r.Method] = c
		}
	}
	in.plan.Store(p)
}

// current returns the published plan. The plan is immutable, so readers need
// no lock and a swap is a single atomic store.
func (in *Injector) current() *plan {
	p, _ := in.plan.Load().(*plan)
	return p
}

// plan is an immutable compiled rule set. It is replaced wholesale on every
// change, never mutated, which is what lets a call read it without a lock.
//
// byIface holds the rules scoped to one interface, keyed by its type name and
// then by method name; anyIface holds the rules that scope to a method only.
// Within either group the empty method name is the wildcard.
type plan struct {
	byIface  map[string]map[string]*compiled
	anyIface map[string]*compiled
}

// match returns the rule that owns a call to method on an interface of the
// given type name, or nil. The lookup walks from the most specific condition
// down; validateSet has already ruled out the one combination that would make
// two of these candidates simultaneously present and incomparable.
func (p *plan) match(iface, method string) *compiled {
	if m := p.byIface[iface]; m != nil {
		if c := m[method]; c != nil {
			return c
		}
		if c := m[""]; c != nil {
			return c
		}
	}
	if c := p.anyIface[method]; c != nil {
		return c
	}
	return p.anyIface[""]
}

// validateSet is Validate. It checks each rule with check, then the set: no
// two rules may claim the same condition, and an interface-scoped wildcard
// must not sit next to a method-scoped one.
func validateSet(rules []Rule) error {
	type condition struct{ iface, method string }
	seen := make(map[condition]int, len(rules))
	ifaceWild, methodWild := "", ""

	for i := range rules {
		r := &rules[i]
		if err := check(*r); err != nil {
			return fmt.Errorf("chaos: rule %d: %w", i, err)
		}
		c := condition{r.Interface, r.Method}
		if j, dup := seen[c]; dup {
			return fmt.Errorf("chaos: rules %d and %d both claim %s", j, i, describe(r.Interface, r.Method))
		}
		seen[c] = i
		if ifaceWild == "" && r.Interface != "" && r.Method == "" {
			ifaceWild = r.Interface
		}
		if methodWild == "" && r.Interface == "" && r.Method != "" {
			methodWild = r.Method
		}
	}
	if ifaceWild != "" && methodWild != "" {
		return fmt.Errorf("chaos: the rule for interface %s with any method and the rule for method %s on any interface overlap, and neither is the more specific; scope one of them further", ifaceWild, methodWild)
	}
	return nil
}

// check returns the reason a single rule could never do anything, or could do
// two contradictory things, or nil.
func check(r Rule) error {
	name := describe(r.Interface, r.Method)
	switch {
	case math.IsNaN(r.Rate) || r.Rate <= 0 || r.Rate > 1:
		return fmt.Errorf("rule for %s has Rate %v; it must be in (0, 1] (use Disable to turn injection off)", name, r.Rate)
	case r.MaxCount < 0:
		return fmt.Errorf("rule for %s has a negative MaxCount", name)
	case !r.From.IsZero() && !r.Until.IsZero() && r.From.After(r.Until):
		return fmt.Errorf("rule for %s has From after Until", name)
	case len(r.Actions) == 0:
		return fmt.Errorf("rule for %s has no actions", name)
	}

	// An action sets exactly one fault, and the one that decides the call is
	// the last: nothing after it could run.
	terminal := false
	for i, a := range r.Actions {
		if a.Latency < 0 {
			return fmt.Errorf("rule for %s, action %d has a negative Latency", name, i)
		}
		n := 0
		if a.Latency > 0 {
			n++
		}
		if a.Err != nil {
			n++
		}
		if panics(a) {
			n++
		}
		if n != 1 {
			return fmt.Errorf("rule for %s, action %d sets %d faults; an action is exactly one", name, i, n)
		}
		if terminal {
			return fmt.Errorf("rule for %s, action %d comes after the action that decides the call; nothing there can run", name, i)
		}
		if a.Err != nil || panics(a) {
			terminal = true
		}
	}
	return nil
}

// describe renders a condition for an error message.
func describe(iface, method string) string {
	if iface == "" {
		iface = "any interface"
	}
	if method == "" {
		method = "any method"
	}
	return "interface " + iface + ", " + method
}

// panics reports whether an action asks for a panic. The field's zero value is
// a nil any, but an any holding an empty string is equally "not set" to anyone
// reading the action.
func panics(a Action) bool {
	if a.Panic == nil {
		return false
	}
	s, ok := a.Panic.(string)
	return !ok || s != ""
}

// Wrap returns a proxy for T that applies the injector's rules to its calls.
//
// A nil target is allowed; the proxy then returns zero values as a mock does,
// with the faults layered on top.
func Wrap[T any](in *Injector, target T) T {
	typ := reflect.TypeOf((*T)(nil)).Elem()
	if typ.Kind() != reflect.Interface {
		panic("chaos.Wrap: T must be an interface type, got " + typ.String())
	}
	return weave.As[T](in.WrapOf(typ, target))
}

// WrapOf is the reflection based form of Wrap, for callers that only have a
// reflect.Type — a generic wrapper function, say. Pass the result to weave.As
// to get a usable interface value.
func (in *Injector) WrapOf(iface reflect.Type, target any) *weave.Proxy {
	if iface.Kind() != reflect.Interface {
		panic("chaos.WrapOf: not an interface type: " + iface.String())
	}
	b := &binder{in: in, ifaceName: iface.Name()}
	// A nil interface reaches weave as an untyped nil and means "no target";
	// Go flattens interfaces on assignment, so a nil T boxed into an any
	// arrives here as nil rather than as a typed nil. A nil pointer, on the
	// other hand, is a receiver, and stays one.
	p := weave.NewOf(iface, target, b.intercept)
	// The method list comes from the proxy rather than from reflect: it is in
	// itab slot order, which is exactly what Invocation.Method.Index indexes.
	// The proxy cannot have been called yet, so the write needs no barrier.
	b.methods = p.Methods()
	return p
}
