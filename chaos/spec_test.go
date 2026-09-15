package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// rulesFrom is the caller's half of the boundary: the package does not fetch
// or decode, so this is what a control plane integration looks like. Every
// Spec goes through Rule and the set through Validate, which is what makes the
// Set below unable to panic.
func rulesFrom(t *testing.T, body string) []Rule {
	t.Helper()
	var specs []Spec
	if err := json.Unmarshal([]byte(body), &specs); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	rules := make([]Rule, len(specs))
	for i, s := range specs {
		r, err := s.Rule()
		if err != nil {
			t.Fatalf("rule %d of %s: %v", i, body, err)
		}
		rules[i] = r
	}
	if err := Validate(rules...); err != nil {
		t.Fatalf("validate %s: %v", body, err)
	}
	return rules
}

// TestTakeoverIsIndependentOfInjection is the contract the split exists for:
// the proxy is installed once, and everything about what it does arrives
// afterwards from somewhere else.
func TestTakeoverIsIndependentOfInjection(t *testing.T) {
	inj := New()
	impl := &repoImpl{}
	r := Wrap[Repo](inj, impl)

	if got, err := r.GetUser(context.Background(), 1); got != "user" || err != nil {
		t.Fatalf("GetUser = %q, %v; want the target's own result", got, err)
	}

	body := `[{"method": "GetUser", "rate": 1, "actions": [{"error": "gateway timeout"}]}]`
	inj.Set(rulesFrom(t, body)...)
	_, err := r.GetUser(context.Background(), 1)
	if !errors.Is(err, ErrInjected) {
		t.Fatalf("err = %v; want an injected error", err)
	}
	if !strings.Contains(err.Error(), "gateway timeout") {
		t.Fatalf("err = %v; want the message from the document", err)
	}
	if impl.n() != 1 {
		t.Fatalf("target saw %d calls; the injected error must not reach it", impl.n())
	}

	inj.Set(rulesFrom(t, `[]`)...)
	if got, err := r.GetUser(context.Background(), 1); got != "user" || err != nil {
		t.Fatalf("GetUser = %q, %v; want the target's own result again", got, err)
	}
}

func TestSpecRuleConvertsActions(t *testing.T) {
	r, err := Spec{
		Method: "GetUser",
		Rate:   0.5,
		Actions: []SpecAction{
			{Latency: "500ms"},
			{Error: "disk full"},
		},
	}.Rule()
	if err != nil {
		t.Fatalf("Rule: %v", err)
	}
	if r.Method != "GetUser" || r.Rate != 0.5 {
		t.Fatalf("rule = %+v; want the spec's method and rate", r)
	}
	if len(r.Actions) != 2 {
		t.Fatalf("actions = %v; want two", r.Actions)
	}
	if r.Actions[0].Latency != 500*time.Millisecond {
		t.Fatalf("action 0 latency = %v; want 500ms", r.Actions[0].Latency)
	}
	if !errors.Is(r.Actions[1].Err, ErrInjected) {
		t.Fatalf("action 1 err = %v; want it to satisfy errors.Is(err, ErrInjected)", r.Actions[1].Err)
	}
	if !strings.Contains(r.Actions[1].Err.Error(), "disk full") {
		t.Fatalf("action 1 err = %v; want the message from the spec", r.Actions[1].Err)
	}
}

func TestSpecRulePanicAction(t *testing.T) {
	r, err := Spec{
		Method:  "Ping",
		Rate:    1,
		Actions: []SpecAction{{Panic: "boom"}},
	}.Rule()
	if err != nil {
		t.Fatalf("Rule: %v", err)
	}
	if len(r.Actions) != 1 || !panics(r.Actions[0]) {
		t.Fatalf("actions = %v; want one panic", r.Actions)
	}
}

func TestSpecRuleLeavesUnsetFieldsAlone(t *testing.T) {
	r, err := Spec{
		Method:  "GetUser",
		Rate:    1,
		Actions: []SpecAction{{Error: "x"}},
	}.Rule()
	if err != nil {
		t.Fatalf("Rule: %v", err)
	}
	if len(r.Actions) != 1 {
		t.Fatalf("actions = %v; want one", r.Actions)
	}
	if r.Actions[0].Latency != 0 {
		t.Fatalf("latency = %v; want 0", r.Actions[0].Latency)
	}
	if panics(r.Actions[0]) {
		t.Fatalf("action = %+v; want no panic", r.Actions[0])
	}
}

func TestSpecRuleRejectsUnusableSpecs(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
	}{
		{"missing rate", Spec{Method: "GetUser", Actions: []SpecAction{{Error: "x"}}}},
		{"rate zero", Spec{Method: "GetUser", Rate: 0, Actions: []SpecAction{{Error: "x"}}}},
		{"rate above one", Spec{Method: "GetUser", Rate: 2, Actions: []SpecAction{{Error: "x"}}}},
		{"bad duration", Spec{Method: "GetUser", Rate: 1, Actions: []SpecAction{{Latency: "soon"}}}},
		{"bad from", Spec{Method: "GetUser", Rate: 1, Actions: []SpecAction{{Error: "x"}}, From: "soon"}},
		{"bad until", Spec{Method: "GetUser", Rate: 1, Actions: []SpecAction{{Error: "x"}}, Until: "soon"}},
		{"no actions", Spec{Method: "GetUser", Rate: 1}},
		{"action sets no fault", Spec{Method: "GetUser", Rate: 1, Actions: []SpecAction{{}}}},
		{"action sets two faults", Spec{Method: "GetUser", Rate: 1, Actions: []SpecAction{{Latency: "1ms", Error: "x"}}}},
		{"action after the deciding one", Spec{Method: "GetUser", Rate: 1, Actions: []SpecAction{{Error: "x"}, {Latency: "1ms"}}}},
	}
	for _, tc := range cases {
		if _, err := tc.spec.Rule(); err == nil {
			t.Fatalf("%s: Rule accepted %+v", tc.name, tc.spec)
		}
	}
}

func TestSpecRuleWindowAndBudget(t *testing.T) {
	from := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	until := from.Add(time.Hour)
	r, err := Spec{
		Interface: "UserRepo",
		Method:    "GetUser",
		Rate:      1,
		Actions:   []SpecAction{{Error: "x"}},
		MaxCount:  7,
		From:      from.Format(time.RFC3339),
		Until:     until.Format(time.RFC3339),
	}.Rule()
	if err != nil {
		t.Fatalf("Rule: %v", err)
	}
	if r.Interface != "UserRepo" || r.MaxCount != 7 {
		t.Fatalf("rule = %+v; want the interface and the budget carried over", r)
	}
	if !r.From.Equal(from) || !r.Until.Equal(until) {
		t.Fatalf("window = %v..%v; want %v..%v", r.From, r.Until, from, until)
	}
}

func TestRulesReturnsACopy(t *testing.T) {
	inj := New(Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}})

	got := inj.Rules()
	if len(got) != 1 || got[0].Method != "GetUser" {
		t.Fatalf("Rules = %v; want the rule passed to New", got)
	}
	got[0].Rate = 999
	if inj.Rules()[0].Rate == 999 {
		t.Fatal("Rules handed out the live slice")
	}
}

func TestRulesReadsBackWhatSetInstalled(t *testing.T) {
	inj := New()
	inj.Set(Rule{Method: "Save", Rate: 0.5, Actions: []Action{Delay(20 * time.Millisecond)}})

	got := inj.Rules()
	if len(got) != 1 || got[0].Method != "Save" || got[0].Rate != 0.5 {
		t.Fatalf("Rules = %v; want what Set installed", got)
	}
	if len(got[0].Actions) != 1 || got[0].Actions[0].Latency != 20*time.Millisecond {
		t.Fatalf("Rules = %v; want the action Set installed", got)
	}
}
