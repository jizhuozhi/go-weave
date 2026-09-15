# chaos — runtime fault injection for Go interfaces

`chaos` injects faults into any interface at runtime: a delay, an error or a
panic, applied to a fraction of the calls that match a rule. It is a single
go-weave interceptor, so it works on third-party and source-less interfaces and
needs no codegen.

```go
inj := chaos.New(
    chaos.Rule{
        Method:  "GetUser",
        Rate:    1,
        Actions: []chaos.Action{chaos.Delay(500 * time.Millisecond)},
    },
    chaos.Rule{
        Method:  "ListUsers",
        Rate:    0.1,
        Actions: []chaos.Action{chaos.Fail(errInjected)},
    },
)
defer inj.Disable()

repo := chaos.Wrap[UserRepo](inj, realRepo)
svc := chaos.Wrap[UserService](inj, realSvc)
```

One injector wraps any number of interfaces. A rule names a method, and a rule
naming a method an interface does not have is ignored for that interface —
which is what lets a single rule set span a whole service.

## Taking over is separate from injecting

Wrapping an interface installs a control point. What passes through it is
decided separately, so an injector can be created with no rules at all and its
rule set replaced at any time:

```go
inj := chaos.New()                       // install the control point
repo := chaos.Wrap[UserRepo](inj, realRepo)

inj.Set(rules...)                        // whatever decides the rules
inj.Disable()                            // and whatever stops them
inj.Rules()                              // read back what is running
```

The package owns the rule set and its lifecycle, not where the rules come from.
Reaching the control plane, and speaking whatever it speaks, belongs to the code
that owns that connection.

## Rules

| Field | Meaning |
| --- | --- |
| `Interface` | interface type name to match; `""` matches every interface |
| `Method` | method name to match; `""` matches every method |
| `Rate` | fraction of matching calls that get the fault, in `(0, 1]` |
| `Actions` | the faults to apply, in order; each is exactly one of a delay, an error or a panic |
| `MaxCount` | stops the rule after that many faults, counted across every interface it owns; `0` means no limit |
| `From`, `Until` | the window in which the rule is active; either may be the zero time for open-ended |

An `Action` is exactly one fault, built with `chaos.Delay`, `chaos.Fail` or
`chaos.Panic`. A rule that wants several faults carries several actions, and
they run in order. A delay stacks, so `Delay` may be followed by anything; the
action that decides the call — `Fail` or `Panic` — has to be the last one,
because nothing after it could run. A rule needs at least one action, and `Rate`
must be in `(0, 1]` — `Disable` is how injection is turned off, not a rate of
zero. `New` and `Set` panic on a rule set that breaks any of that, or whose
conditions are not a strict partial order: a rule that could never fire, or two
rules that both claim the same calls, are mistakes worth failing on at setup
rather than discovering from a fault that never happened — or from two faults
where one was expected.

`Fail` needs somewhere to go. Its error is written to the method's `error`
result, and a method with no such result is unaffected by it — its other
actions still apply.

`Interface` is matched against the interface type's name
(`reflect.Type.Name`), so it is a short name like `"UserRepo"`, not a path. Two
interfaces with the same type name are indistinguishable to it.

## Matching

A rule's condition is `Interface` and `Method`, either of which may be empty —
empty matches everything on that axis. Four shapes exist:

```text
("UserRepo", "GetUser")   one method of one interface
("UserRepo", "")          every method of one interface
("", "GetUser")           one method of every interface
("", "")                  everything
```

Conditions are ordered by specificity, and a rule set has to be a **strict
partial order** under that relation. `Validate` — and through it `New` and
`Set` — rejects a set where that does not hold. Two pairs can break it:

- two rules with the same condition, which would both claim the same calls;
- a rule scoped to an interface with a wildcard method, next to a rule scoped
  to a method with a wildcard interface. `("Repo", "")` and `("", "Save")`
  overlap without either being the more specific, so nothing says which of them
  a call to `Repo.Save` belongs to.

Everything else is either disjoint or comparable, and the more specific rule
owns the call. What the strictness buys is the invariant the package is built
around: **exactly one rule owns every call.** There is no priority tie to
reason about, and no "last declared wins".

A rule that owns a call but is outside its window, or past its `MaxCount`,
still owns it — a less specific rule does not step in. The call is simply not
faulted.

Resolution happens in three steps, and only the first is proportional to the
number of rules:

1. **`Set` compiles.** Rules are split into a map per interface name and one
   map for the interface-agnostic rules, keyed by method name. `O(rules)`,
   once per push.
2. **Each interface resolves once.** For every method of the interface the
   owning rule is looked up — at most four map lookups, walking from the most
   specific condition down — and written into a table indexed by interface
   method index. `O(methods)`, once per rule-set change.
3. **Each call indexes.** The table is indexed by `Invocation.Method.Index`,
   which weave hands over for free as the itab slot number. `O(1)`: no loop, no
   string comparison, no map lookup.

A call does not scan the rule list. The list is compiled down to an array index
before any call sees it, so 501 rules cost the same per call as one, and a
changed rule set is recompiled once rather than re-examined on every call.

## Spec

`Spec` is a rule as data — the shape a control plane sends and a source decodes
into:

```json
[
  {"interface": "UserRepo", "method": "GetUser", "rate": 1,
   "actions": [{"latency": "500ms"}]},
  {"method": "Save", "rate": 0.01, "actions": [{"error": "disk full"}],
   "maxCount": 100, "from": "2026-09-15T10:00:00Z", "until": "2026-09-15T11:00:00Z"}
]
```

| Field | Meaning |
| --- | --- |
| `interface` | interface type name to match; empty matches every interface |
| `method` | method name to match; empty matches every method |
| `rate` | fraction of matching calls, in `(0, 1]` — required, no default |
| `actions` | the faults to apply, in order; each is exactly one of `{"latency": "500ms"}`, `{"error": "..."}` or `{"panic": "..."}` |
| `maxCount` | stop after this many faults; `0` means no limit |
| `from`, `until` | RFC3339 timestamps bounding the window |

Decoding is not this package's job, so the whole of a control plane
integration is yours and looks like this:

```go
func rulesFrom(body []byte) ([]chaos.Rule, error) {
    var specs []chaos.Spec
    if err := json.Unmarshal(body, &specs); err != nil {
        return nil, err
    }
    rules := make([]chaos.Rule, len(specs))
    for i, s := range specs {
        r, err := s.Rule()
        if err != nil {
            return nil, err        // a rejected set changes nothing
        }
        rules[i] = r
    }
    if err := chaos.Validate(rules...); err != nil {
        return nil, err            // a conflicting set changes nothing either
    }
    return rules, nil
}
```

`Spec.Rule` is the one place the mapping onto a `Rule` is defined, so every
source agrees on what a field means and on what makes a single rule unusable.
`Validate` is the one place the set-level rule is defined — the partial order
among conditions. Both report through an error rather than panicking, which is
what makes `Set` safe to call with their output: a rule set that has been
through both cannot panic.

`rate` has no default on purpose: reading a missing field as "every call" is
the one mistake a fault injector must not make quietly.

An error injected from a `Spec` is wrapped, so a caller can tell a fault from a
real failure:

```go
if errors.Is(err, chaos.ErrInjected) { ... }
```

An error injected from a programmatic `Rule` is used verbatim instead, which is
what lets a test simulate one specific error value.

## The kill switch

```go
inj.Disable()      // takes effect on the next call
inj.Enable()
inj.Set(rules...)  // replaces the rule set and enables
```

All three are safe while calls are in flight. `Set` validates before it
mutates, so a rejected rule set leaves the previous one running.

## Cost

Rules are compiled once per interface into a table indexed by interface method
index, so a call that matches no rule costs an array index on top of the
proxy's own fast path. Best of five runs each, same machine:

```sh
BenchmarkProxyAdd (bare proxy)   79 ns/op    0 allocs/op
BenchmarkUnmatchedCall           85 ns/op    0 allocs/op
BenchmarkDisabledInjector        86 ns/op    0 allocs/op
BenchmarkMatchedButNotFaulted    91 ns/op    0 allocs/op
```

The spread across runs is wider than the gaps between those rows, so read them
as one claim: an injector adds no allocation and no measurable per-call cost to
a method it does not fault, and a disabled one adds nothing at all.

Injecting a latency is the only case that can pull a call off the fast path,
and only when the method takes a `context.Context`: the wait then selects on
the context, so a caller with a deadline is not held past it. A delay that
ignored cancellation would hold a caller past its own deadline — the behaviour
the experiment exists to surface, not one it should introduce. When the context
ends first, the delay stops, the call proceeds, and the target sees the
cancelled context itself. Only argument types are inspected to find the
context, so a method without one never materialises its arguments.

## Sampling

Which calls get the fault is deterministic: the k'th matching call is faulted
when a hash of k falls below `Rate`. A given call sequence therefore always
produces the same fault pattern, which makes an experiment reproducible without
a seeded RNG, and keeps the hot path free of shared RNG state — no lock, no
global counter.

## Limitations

- **`Set` replaces, it does not merge.** What a control plane sends is the
  whole rule set, with no per-rule add or remove. A platform that manages
  individual experiments has to send the merged set.
- **`Panic` unwinds through the caller.** weave recovers nothing, so an
  injected panic reaches your code exactly as one from the target would. That
  is deliberate, and it means a panic fault is only as contained as the call
  site that catches it.
- **`Fail` only reaches an `error` result.** A method returning a concrete error
  type, or no error at all, cannot carry an injected error.
- **Matching is on names, and on nothing else.** A condition is an interface
  type name and a method name; there is no matching on arguments or on any
  other property of the call. Two interfaces that share a type name are
  indistinguishable, and every call a rule owns is subject to the same rate.
- **Sampling state is per proxy.** Every `Wrap` starts its own call counters,
  so two proxies of the same interface sample independently.
