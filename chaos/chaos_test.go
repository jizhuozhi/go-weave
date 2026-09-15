package chaos

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	weave "github.com/jizhuozhi/go-weave"
)

var errInjected = errors.New("injected")

type Repo interface {
	GetUser(ctx context.Context, id int64) (string, error)
	Save(ctx context.Context, name string) error
	ListUsers(limit int) []string
	Ping()
	Count() int
}

type repoImpl struct {
	mu    sync.Mutex
	calls int
}

func (r *repoImpl) GetUser(ctx context.Context, id int64) (string, error) {
	r.bump()
	return "user", nil
}

func (r *repoImpl) Save(ctx context.Context, name string) error {
	r.bump()
	return nil
}

func (r *repoImpl) ListUsers(limit int) []string { return []string{"a", "b"} }
func (r *repoImpl) Ping()                        {}
func (r *repoImpl) Count() int                   { return 7 }

func (r *repoImpl) bump() {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
}

func (r *repoImpl) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type Greeter interface {
	Hello(name string) (string, error)
}

type greeterImpl struct{}

func (greeterImpl) Hello(name string) (string, error) { return "hi " + name, nil }

func mustPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: expected a panic", what)
		}
	}()
	f()
}

func TestNoRuleLeavesCallsAlone(t *testing.T) {
	impl := &repoImpl{}
	r := Wrap[Repo](New(), impl)

	got, err := r.GetUser(context.Background(), 1)
	if got != "user" || err != nil {
		t.Fatalf("GetUser = %q, %v; want \"user\", nil", got, err)
	}
	if n := r.Count(); n != 7 {
		t.Fatalf("Count = %d; want 7", n)
	}
	if impl.n() != 1 {
		t.Fatalf("target saw %d calls; want 1", impl.n())
	}
}

func TestLatencyIsSleptBeforeTheCall(t *testing.T) {
	r := Wrap[Repo](New(Rule{Method: "ListUsers", Rate: 1, Actions: []Action{Delay(60 * time.Millisecond)}}), &repoImpl{})

	start := time.Now()
	r.ListUsers(3)
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("call returned after %v; want at least 60ms", d)
	}

	// The fault is scoped to the method it names.
	start = time.Now()
	if n := r.Count(); n != 7 || time.Since(start) > 20*time.Millisecond {
		t.Fatal("Count was affected by a rule naming ListUsers")
	}
}

func TestLatencyStopsWhenTheContextIsDone(t *testing.T) {
	r := Wrap[Repo](New(Rule{Method: "GetUser", Rate: 1, Actions: []Action{Delay(2 * time.Second)}}), &repoImpl{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := r.GetUser(ctx, 1); err != nil {
		t.Fatalf("err = %v; want the target's own result", err)
	}
	d := time.Since(start)
	if d > 500*time.Millisecond {
		t.Fatalf("call held for %v; the delay ignored context cancellation", d)
	}
	if d < 30*time.Millisecond {
		t.Fatalf("call returned after %v; the delay did not run", d)
	}
}

func TestErrorShortCircuits(t *testing.T) {
	impl := &repoImpl{}
	r := Wrap[Repo](New(Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}}), impl)

	got, err := r.GetUser(context.Background(), 1)
	if !errors.Is(err, errInjected) {
		t.Fatalf("err = %v; want %v", err, errInjected)
	}
	if got != "" {
		t.Fatalf("result = %q; want the zero value alongside the injected error", got)
	}
	if impl.n() != 0 {
		t.Fatalf("target was called %d times; an injected error must not reach it", impl.n())
	}
}

func TestErrorOnAMethodWithoutAnErrorResult(t *testing.T) {
	r := Wrap[Repo](New(Rule{Method: "ListUsers", Rate: 1, Actions: []Action{Fail(errInjected)}}), &repoImpl{})

	// ListUsers has no error result, so an Err-only rule has nothing to
	// inject and the call goes through untouched.
	if got := r.ListUsers(2); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("ListUsers = %v; want the target's result", got)
	}
}

func TestPanicUnwindsThroughTheCaller(t *testing.T) {
	r := Wrap[Repo](New(Rule{Method: "Ping", Rate: 1, Actions: []Action{Panic("boom")}}), &repoImpl{})

	defer func() {
		if v := recover(); v != "boom" {
			t.Fatalf("recovered %v; want \"boom\"", v)
		}
	}()
	r.Ping()
	t.Fatal("Ping returned; the injected panic did not fire")
}

func TestLatencyAndErrorCombine(t *testing.T) {
	r := Wrap[Repo](New(Rule{
		Method: "GetUser",
		Rate:   1,
		Actions: []Action{
			Delay(40 * time.Millisecond),
			Fail(errInjected),
		},
	}), &repoImpl{})

	start := time.Now()
	_, err := r.GetUser(context.Background(), 1)
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Fatalf("call returned after %v; want the delay first", d)
	}
	if !errors.Is(err, errInjected) {
		t.Fatalf("err = %v; want %v", err, errInjected)
	}
}

// TestRateIsDeterministic pins the sampling contract: the same call sequence
// always faults the same calls, so an experiment is reproducible.
func TestRateIsDeterministic(t *testing.T) {
	count := func() int {
		r := Wrap[Repo](New(Rule{Method: "GetUser", Rate: 0.25, Actions: []Action{Fail(errInjected)}}), &repoImpl{})
		n := 0
		for i := 0; i < 400; i++ {
			if _, err := r.GetUser(context.Background(), int64(i)); err != nil {
				n++
			}
		}
		return n
	}
	first, second := count(), count()
	if first != second {
		t.Fatalf("sampling is not reproducible: %d vs %d faults", first, second)
	}
	if first == 0 || first == 400 {
		t.Fatalf("faulted %d of 400 calls; want a fraction of them", first)
	}
}

func TestRateApproximatesTheFraction(t *testing.T) {
	r := Wrap[Repo](New(Rule{Method: "GetUser", Rate: 0.25, Actions: []Action{Fail(errInjected)}}), &repoImpl{})

	const n = 2000
	faulted := 0
	for i := 0; i < n; i++ {
		if _, err := r.GetUser(context.Background(), int64(i)); err != nil {
			faulted++
		}
	}
	// A quarter of 2000 is 500. The band is wide on purpose: what matters is
	// that it is a fraction rather than all or nothing.
	if faulted < 400 || faulted > 600 {
		t.Fatalf("faulted %d of %d calls; want about 500", faulted, n)
	}
}

func TestRateOneFaultsEveryCall(t *testing.T) {
	r := Wrap[Repo](New(Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}}), &repoImpl{})
	for i := 0; i < 50; i++ {
		if _, err := r.GetUser(context.Background(), 1); !errors.Is(err, errInjected) {
			t.Fatalf("call %d was not faulted: %v", i, err)
		}
	}
}

func TestDisableAndEnable(t *testing.T) {
	inj := New(Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}})
	r := Wrap[Repo](inj, &repoImpl{})

	if _, err := r.GetUser(context.Background(), 1); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v; want the fault while enabled", err)
	}

	inj.Disable()
	if inj.Enabled() {
		t.Fatal("Enabled reported true after Disable")
	}
	if _, err := r.GetUser(context.Background(), 1); err != nil {
		t.Fatalf("err = %v; want no fault while disabled", err)
	}

	inj.Enable()
	if _, err := r.GetUser(context.Background(), 1); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v; want the fault back after Enable", err)
	}
}

func TestSetReplacesTheRuleSet(t *testing.T) {
	inj := New(Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}})
	r := Wrap[Repo](inj, &repoImpl{})

	if _, err := r.GetUser(context.Background(), 1); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v; want the original rule", err)
	}

	inj.Set(Rule{Method: "GetUser", Rate: 1, Actions: []Action{Delay(30 * time.Millisecond)}})
	start := time.Now()
	if _, err := r.GetUser(context.Background(), 1); err != nil {
		t.Fatalf("err = %v; the replaced rule still fires", err)
	}
	if d := time.Since(start); d < 30*time.Millisecond {
		t.Fatalf("call returned after %v; the new rule did not apply", d)
	}
}

func TestSetWithAnInvalidRuleKeepsTheOldOne(t *testing.T) {
	inj := New(Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}})
	r := Wrap[Repo](inj, &repoImpl{})

	mustPanic(t, "Set with Rate 0", func() {
		inj.Set(Rule{Method: "Save", Rate: 0, Actions: []Action{Fail(errInjected)}})
	})
	if _, err := r.GetUser(context.Background(), 1); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v; a rejected Set must leave the old rules in place", err)
	}
}

func TestNamedRuleOutranksWildcard(t *testing.T) {
	r := Wrap[Repo](New(
		Rule{Method: "", Rate: 1, Actions: []Action{Fail(errInjected)}},
		Rule{Method: "GetUser", Rate: 1, Actions: []Action{Delay(30 * time.Millisecond)}},
	), &repoImpl{})

	start := time.Now()
	got, err := r.GetUser(context.Background(), 1)
	if err != nil {
		t.Fatalf("err = %v; the wildcard rule should have lost", err)
	}
	if d := time.Since(start); d < 30*time.Millisecond {
		t.Fatalf("call returned after %v; the named rule did not apply", d)
	}
	if got != "user" {
		t.Fatalf("GetUser = %q; want \"user\"", got)
	}

	// The wildcard still covers everything the named rule does not.
	if err := r.Save(context.Background(), "x"); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v; the wildcard rule should cover Save", err)
	}
}

// TestARuleSetReachesEveryWrappedInterface is the injector's side of the
// contract: whatever rule set it is given is applied to every interface taken
// over through it, and replaced for all of them at once.
func TestARuleSetReachesEveryWrappedInterface(t *testing.T) {
	inj := New()
	repo := Wrap[Repo](inj, &repoImpl{})
	greeter := Wrap[Greeter](inj, greeterImpl{})

	// A rule set installed after both were wrapped reaches both.
	inj.Set(
		Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}},
		Rule{Method: "Hello", Rate: 1, Actions: []Action{Fail(errInjected)}},
	)
	if _, err := repo.GetUser(context.Background(), 1); !errors.Is(err, errInjected) {
		t.Fatalf("repo err = %v; want the fault", err)
	}
	if _, err := greeter.Hello("ada"); !errors.Is(err, errInjected) {
		t.Fatalf("greeter err = %v; want the fault", err)
	}

	// Replacing it reaches both again.
	inj.Set(Rule{Method: "GetUser", Rate: 1, Actions: []Action{Delay(30 * time.Millisecond)}})
	if _, err := repo.GetUser(context.Background(), 1); err != nil {
		t.Fatalf("repo err = %v; the replaced rule set still fires", err)
	}
	if _, err := greeter.Hello("ada"); err != nil {
		t.Fatalf("greeter err = %v; the replaced rule set still fires", err)
	}
}

func TestNilTargetStillInjects(t *testing.T) {
	inj := New(Rule{Method: "Hello", Rate: 1, Actions: []Action{Fail(errInjected)}})
	greeter := Wrap[Greeter](inj, nil)

	if _, err := greeter.Hello("ada"); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v; want the fault over a nil target", err)
	}

	inj.Disable()
	got, err := greeter.Hello("ada")
	if got != "" || err != nil {
		t.Fatalf("Hello = %q, %v; want the zero values of a targetless proxy", got, err)
	}
}

func TestWrapOfReflectionForm(t *testing.T) {
	inj := New(Rule{Method: "Hello", Rate: 1, Actions: []Action{Fail(errInjected)}})
	iface := reflect.TypeOf((*Greeter)(nil)).Elem()

	p := inj.WrapOf(iface, greeterImpl{})
	if _, err := weave.As[Greeter](p).Hello("ada"); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v; want the fault", err)
	}
}

func TestWrapRejectsNonInterfaces(t *testing.T) {
	mustPanic(t, "Wrap with a concrete type", func() {
		Wrap[*repoImpl](New(), &repoImpl{})
	})
	mustPanic(t, "WrapOf with a concrete type", func() {
		New().WrapOf(reflect.TypeOf(0), nil)
	})
}

func TestRuleValidation(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		rule Rule
	}{
		{"rate zero", Rule{Method: "GetUser", Rate: 0, Actions: []Action{Fail(errInjected)}}},
		{"rate above one", Rule{Method: "GetUser", Rate: 1.5, Actions: []Action{Fail(errInjected)}}},
		{"negative latency", Rule{Method: "GetUser", Rate: 1, Actions: []Action{Delay(-time.Second)}}},
		{"negative max count", Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}, MaxCount: -1}},
		{"from after until", Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}, From: now, Until: now.Add(-time.Minute)}},
		{"action sets two faults", Rule{Method: "GetUser", Rate: 1, Actions: []Action{{Err: errInjected, Panic: "x"}}}},
		{"action after the deciding one", Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected), Delay(time.Millisecond)}}},
		{"no actions", Rule{Method: "GetUser", Rate: 1}},
	}
	for _, tc := range cases {
		mustPanic(t, tc.name, func() { New(tc.rule) })
	}
}

// --- interface scoping -----------------------------------------------------

func TestInterfaceScoping(t *testing.T) {
	inj := New(Rule{Interface: "Repo", Method: "Save", Rate: 1, Actions: []Action{Fail(errInjected)}})
	repo := Wrap[Repo](inj, &repoImpl{})
	greeter := Wrap[Greeter](inj, greeterImpl{})

	if err := repo.Save(context.Background(), "x"); !errors.Is(err, errInjected) {
		t.Fatalf("repo.Save err = %v; want the fault", err)
	}
	if _, err := repo.GetUser(context.Background(), 1); err != nil {
		t.Fatalf("repo.GetUser err = %v; the rule names Save", err)
	}
	if _, err := greeter.Hello("ada"); err != nil {
		t.Fatalf("greeter err = %v; a rule scoped to Repo must not reach Greeter", err)
	}
}

func TestInterfaceScopedRuleOutranksMethodScoped(t *testing.T) {
	inj := New(
		Rule{Method: "Save", Rate: 1, Actions: []Action{Fail(errInjected)}},
		Rule{Interface: "Repo", Method: "Save", Rate: 1, Actions: []Action{Delay(30 * time.Millisecond)}},
	)
	repo := Wrap[Repo](inj, &repoImpl{})

	start := time.Now()
	if err := repo.Save(context.Background(), "x"); err != nil {
		t.Fatalf("err = %v; the interface-scoped rule should own the call", err)
	}
	if d := time.Since(start); d < 30*time.Millisecond {
		t.Fatalf("call returned after %v; the interface-scoped rule did not apply", d)
	}
}

func TestRuleForAnUnknownInterfaceIsIgnored(t *testing.T) {
	r := Wrap[Repo](New(Rule{Interface: "NothingHasThisName", Method: "Save", Rate: 1, Actions: []Action{Fail(errInjected)}}), &repoImpl{})
	if err := r.Save(context.Background(), "x"); err != nil {
		t.Fatalf("err = %v; a rule scoped to an interface that was never wrapped must not fire", err)
	}
}

// --- the strict partial order ----------------------------------------------

func TestDuplicateConditionsAreRejected(t *testing.T) {
	mustPanic(t, "two rules for one method", func() {
		New(
			Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}},
			Rule{Method: "GetUser", Rate: 0.5, Actions: []Action{Delay(time.Millisecond)}},
		)
	})
	mustPanic(t, "two rules for one condition", func() {
		New(
			Rule{Interface: "Repo", Rate: 1, Actions: []Action{Fail(errInjected)}},
			Rule{Interface: "Repo", Rate: 0.5, Actions: []Action{Delay(time.Millisecond)}},
		)
	})
}

func TestIncomparableOverlapIsRejected(t *testing.T) {
	mustPanic(t, "interface wildcard next to method wildcard", func() {
		New(
			Rule{Interface: "Repo", Rate: 1, Actions: []Action{Fail(errInjected)}},
			Rule{Method: "Save", Rate: 0.5, Actions: []Action{Delay(time.Millisecond)}},
		)
	})
}

func TestComparableOverlapIsAccepted(t *testing.T) {
	// Every set below overlaps somewhere, but each pair is comparable, so each
	// is a strict partial order and must be accepted.
	sets := [][]Rule{
		{
			Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}},
			Rule{Interface: "Repo", Method: "GetUser", Rate: 1, Actions: []Action{Delay(time.Millisecond)}},
		},
		{
			Rule{Rate: 1, Actions: []Action{Fail(errInjected)}},
			Rule{Interface: "Repo", Rate: 1, Actions: []Action{Delay(time.Millisecond)}},
		},
		{
			Rule{Rate: 1, Actions: []Action{Fail(errInjected)}},
			Rule{Method: "GetUser", Rate: 1, Actions: []Action{Delay(time.Millisecond)}},
		},
		{
			Rule{Interface: "Repo", Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}},
			Rule{Interface: "Greeter", Method: "GetUser", Rate: 1, Actions: []Action{Delay(time.Millisecond)}},
		},
	}
	for i, rules := range sets {
		if err := Validate(rules...); err != nil {
			t.Fatalf("set %d rejected: %v", i, err)
		}
	}
}

func TestValidateReportsInsteadOfPanicking(t *testing.T) {
	err := Validate(
		Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}},
		Rule{Method: "GetUser", Rate: 0.5, Actions: []Action{Delay(time.Millisecond)}},
	)
	if err == nil {
		t.Fatal("Validate accepted two rules for the same condition")
	}
	if !strings.Contains(err.Error(), "GetUser") {
		t.Fatalf("err = %v; want it to name the condition", err)
	}
}

func TestSetWithAConflictingRuleKeepsTheOldSet(t *testing.T) {
	inj := New(Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}})
	r := Wrap[Repo](inj, &repoImpl{})

	mustPanic(t, "Set with a duplicate condition", func() {
		inj.Set(
			Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}},
			Rule{Method: "GetUser", Rate: 1, Actions: []Action{Delay(time.Millisecond)}},
		)
	})
	if _, err := r.GetUser(context.Background(), 1); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v; a rejected Set must leave the old rules running", err)
	}
}

// --- budget and window -----------------------------------------------------

func TestMaxCountStopsTheRule(t *testing.T) {
	r := Wrap[Repo](New(Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}, MaxCount: 3}), &repoImpl{})

	faulted := 0
	for i := 0; i < 10; i++ {
		if _, err := r.GetUser(context.Background(), 1); err != nil {
			faulted++
		}
	}
	if faulted != 3 {
		t.Fatalf("faulted %d of 10 calls; want exactly 3", faulted)
	}
}

func TestMaxCountBelongsToTheRuleNotTheProxy(t *testing.T) {
	inj := New(Rule{Rate: 1, Actions: []Action{Fail(errInjected)}, MaxCount: 2})
	repo := Wrap[Repo](inj, &repoImpl{})
	greeter := Wrap[Greeter](inj, greeterImpl{})

	faulted := 0
	for i := 0; i < 5; i++ {
		if _, err := repo.GetUser(context.Background(), 1); err != nil {
			faulted++
		}
		if _, err := greeter.Hello("ada"); err != nil {
			faulted++
		}
	}
	if faulted != 2 {
		t.Fatalf("faulted %d calls across two proxies; want 2, the rule's whole budget", faulted)
	}
}

func TestWindowNotYetOpen(t *testing.T) {
	r := Wrap[Repo](New(Rule{
		Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)},
		From: time.Now().Add(time.Hour),
	}), &repoImpl{})
	if _, err := r.GetUser(context.Background(), 1); err != nil {
		t.Fatalf("err = %v; the window has not opened yet", err)
	}
}

func TestWindowExpired(t *testing.T) {
	r := Wrap[Repo](New(Rule{
		Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)},
		Until: time.Now().Add(-time.Hour),
	}), &repoImpl{})
	if _, err := r.GetUser(context.Background(), 1); err != nil {
		t.Fatalf("err = %v; the window has closed", err)
	}
}

func TestWindowOpen(t *testing.T) {
	now := time.Now()
	r := Wrap[Repo](New(Rule{
		Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)},
		From:  now.Add(-time.Minute),
		Until: now.Add(time.Minute),
	}), &repoImpl{})
	if _, err := r.GetUser(context.Background(), 1); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v; the window is open", err)
	}
}

func TestAnInactiveRuleStillOwnsItsCalls(t *testing.T) {
	// The specific rule has expired. It still owns GetUser on Repo, so the
	// wildcard underneath does not step in: a call is either faulted by the
	// rule that owns it or not faulted at all.
	r := Wrap[Repo](New(
		Rule{Rate: 1, Actions: []Action{Fail(errInjected)}},
		Rule{Interface: "Repo", Method: "GetUser", Rate: 1, Actions: []Action{Delay(30 * time.Millisecond)}, Until: time.Now().Add(-time.Hour)},
	), &repoImpl{})

	start := time.Now()
	if _, err := r.GetUser(context.Background(), 1); err != nil {
		t.Fatalf("err = %v; the expired rule let the wildcard take over", err)
	}
	if d := time.Since(start); d >= 30*time.Millisecond {
		t.Fatalf("call took %v; the expired rule still fired", d)
	}
	if err := r.Save(context.Background(), "x"); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v; the wildcard should still own Save", err)
	}
}

// TestConcurrentReconfiguration drives the reconfiguration paths against live
// traffic; run with -race.
func TestConcurrentReconfiguration(t *testing.T) {
	inj := New(Rule{Method: "GetUser", Rate: 0.5, Actions: []Action{Fail(errInjected)}})
	r := Wrap[Repo](inj, &repoImpl{})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r.GetUser(context.Background(), 1)
				r.Count()
				r.ListUsers(1)
				r.Ping()
			}
		}()
	}
	for i := 0; i < 200; i++ {
		inj.Disable()
		inj.Enable()
		inj.Set(Rule{Method: "GetUser", Rate: 0.5, Actions: []Action{Fail(errInjected)}})
	}
	close(stop)
	wg.Wait()
}

// The three benchmarks below measure what an injector costs a method it does
// not fault. The rate in the last one is small enough that no call is actually
// faulted, so it isolates the matching and sampling overhead.

func BenchmarkUnmatchedCall(b *testing.B) {
	r := Wrap[Repo](New(Rule{Method: "GetUser", Rate: 1, Actions: []Action{Fail(errInjected)}}), &repoImpl{})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Count()
	}
}

func BenchmarkDisabledInjector(b *testing.B) {
	inj := New(Rule{Method: "Count", Rate: 1, Actions: []Action{Delay(time.Millisecond)}})
	inj.Disable()
	r := Wrap[Repo](inj, &repoImpl{})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Count()
	}
}

func BenchmarkMatchedButNotFaulted(b *testing.B) {
	r := Wrap[Repo](New(Rule{Method: "Count", Rate: 1e-9, Actions: []Action{Delay(time.Millisecond)}}), &repoImpl{})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Count()
	}
}

// BenchmarkManyRules is the same call with 500 rules configured instead of
// one. Matching is resolved once per interface per rule-set change, so the
// number of rules does not reach the call path at all.
func BenchmarkManyRules(b *testing.B) {
	rules := make([]Rule, 0, 501)
	for i := 0; i < 500; i++ {
		rules = append(rules, Rule{Method: fmt.Sprintf("Method%d", i), Rate: 1, Actions: []Action{Fail(errInjected)}})
	}
	rules = append(rules, Rule{Method: "Count", Rate: 1e-9, Actions: []Action{Delay(time.Millisecond)}})

	r := Wrap[Repo](New(rules...), &repoImpl{})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Count()
	}
}

// BenchmarkSet500Rules measures the other side: compiling a rule set happens
// on the control path, once per push, never per call.
func BenchmarkSet500Rules(b *testing.B) {
	rules := make([]Rule, 0, 500)
	for i := 0; i < 500; i++ {
		rules = append(rules, Rule{Method: fmt.Sprintf("Method%d", i), Rate: 1, Actions: []Action{Fail(errInjected)}})
	}
	inj := New()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		inj.Set(rules...)
	}
}
