package chaos

import (
	"errors"
	"fmt"
	"time"
)

// ErrInjected is wrapped into every error injected from a Spec, so a caller
// can tell a fault apart from a real failure:
//
//	if errors.Is(err, chaos.ErrInjected) {
//		// the control plane made this happen
//	}
//
// An error injected from a programmatic Rule is used verbatim instead, which
// is what lets a test simulate one specific error value.
var ErrInjected = errors.New("chaos: injected fault")

// SpecAction is an action as data: the shape a control plane sends for one
// fault of a rule. Exactly one of its fields is set, exactly as an Action.
type SpecAction struct {
	// Latency is a Go duration string, such as "500ms".
	Latency string `json:"latency,omitempty"`
	// Error is the message of the error to inject, wrapped with ErrInjected.
	Error string `json:"error,omitempty"`
	// Panic is the value to panic with.
	Panic string `json:"panic,omitempty"`
}

// action converts one SpecAction, reporting the reason it is unusable.
func (a SpecAction) action(where string, i int) (Action, error) {
	n := 0
	for _, s := range []string{a.Latency, a.Error, a.Panic} {
		if s != "" {
			n++
		}
	}
	if n != 1 {
		return Action{}, fmt.Errorf("%s, action %d sets %d faults; an action is exactly one", where, i, n)
	}
	switch {
	case a.Latency != "":
		d, err := time.ParseDuration(a.Latency)
		if err != nil {
			return Action{}, fmt.Errorf("%s, action %d: latency %q is not a duration: %w", where, i, a.Latency, err)
		}
		return Delay(d), nil
	case a.Error != "":
		return Fail(fmt.Errorf("%w: %s", ErrInjected, a.Error)), nil
	default:
		return Panic(a.Panic), nil
	}
}

// Spec is a rule as data: the shape a control plane sends and a rule source
// decodes into.
//
//	[
//	  {"interface": "UserRepo", "method": "GetUser", "rate": 1,
//	   "actions": [{"latency": "500ms"}]},
//	  {"method": "Save", "rate": 0.01, "maxCount": 100,
//	   "actions": [{"error": "disk full"}]}
//	]
//
// Nothing here fetches or decodes a Spec. Reaching the control plane, and
// speaking whatever it speaks, belongs to the code that owns that connection;
// this type exists so that every such source agrees on the field names and on
// what they mean.
//
// Rate has no default. A document that omits it is rejected, because the
// alternative — reading a missing field as "every call" — is the one mistake a
// fault injector must not make quietly.
type Spec struct {
	// Interface is the interface type name to match; empty matches every
	// interface.
	Interface string `json:"interface,omitempty"`
	// Method is the method name to match; empty matches every method.
	Method string `json:"method,omitempty"`
	// Rate is the fraction of matching calls that get the fault, in (0, 1].
	Rate float64 `json:"rate"`
	// Actions are the faults the rule applies, in order.
	Actions []SpecAction `json:"actions"`
	// MaxCount stops the rule after that many faults; 0 means no limit.
	MaxCount int `json:"maxCount,omitempty"`
	// From and Until bound when the rule is active, as RFC3339 timestamps.
	From  string `json:"from,omitempty"`
	Until string `json:"until,omitempty"`
}

// Rule converts a Spec, returning the reason it is unusable.
//
// Set panics on a bad rule set; this reports through an error instead. A Spec
// is data from somewhere else, and bad data is a runtime condition rather than
// a bug in the program that received it. Converting every Spec, and passing
// the result through Validate, is therefore enough to guarantee Set cannot
// panic.
func (s Spec) Rule() (Rule, error) {
	r := Rule{
		Interface: s.Interface,
		Method:    s.Method,
		Rate:      s.Rate,
		MaxCount:  s.MaxCount,
	}
	where := describe(s.Interface, s.Method)

	if len(s.Actions) > 0 {
		r.Actions = make([]Action, len(s.Actions))
		for i, a := range s.Actions {
			act, err := a.action(where, i)
			if err != nil {
				return Rule{}, err
			}
			r.Actions[i] = act
		}
	}

	if s.From != "" {
		t, err := time.Parse(time.RFC3339, s.From)
		if err != nil {
			return Rule{}, fmt.Errorf("%s: from %q is not an RFC3339 timestamp: %w", where, s.From, err)
		}
		r.From = t
	}
	if s.Until != "" {
		t, err := time.Parse(time.RFC3339, s.Until)
		if err != nil {
			return Rule{}, fmt.Errorf("%s: until %q is not an RFC3339 timestamp: %w", where, s.Until, err)
		}
		r.Until = t
	}
	if err := check(r); err != nil {
		return Rule{}, err
	}
	return r, nil
}
