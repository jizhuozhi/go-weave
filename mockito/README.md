# mockito — runtime mocks & spies for Go

`mockito` builds a Mockito-style mocking framework on top of
[go-weave](../README.md)'s runtime dynamic proxies. No codegen, no generated
files: an interface value is forged at runtime, so a mock is just
`mockito.Mock[T]()` — and a spy is the same thing over a real target.

It targets the three things gomock does badly: **spies** (partial mocks),
**mocking third-party / source-less interfaces**, and **deciding behavior at
runtime**.

## Quick start

```go
type Greeter interface {
    Hello(name string) string
    Greet(name string) (string, error)
}

m := mockito.Mock[Greeter]()

mockito.When(m.Hello("ada")).ThenReturn("hi ada")

m.Hello("ada")                 // "hi ada"
m.Hello("bob")                 // ""  (zero value)

mockito.Verify(m.Hello("ada")).Once()
```

## Mock vs Spy

| | `Mock[T]()` | `Spy[T](target)` |
|---|---|---|
| unstubbed calls | zero values | the real target |
| stubbed calls | your return values | your return values |
| purpose | isolate a dependency | partially override a real object |

```go
s := mockito.Spy[Greeter](realGreeter{})
s.Hello("ada")                       // "hello, ada"  (real)
mockito.When(s.Hello("bob")).ThenReturn("hi bob")
s.Hello("bob")                       // "hi bob"      (overridden)
```

## Stubbing

Stubbing is *recording-style*: the arguments are a real call, so they are
type-checked at compile time, and the number of results selects the right
`When`:

```go
mockito.When(m.M())                    // 1 result
mockito.When2(m.M())                   // 2 results
mockito.When3(m.M())                   // 3 results
mockito.When4(m.M())                   // 4 results

mockito.When2(m.Greet("ada")).ThenReturn("hi ada", nil)
```

A `nil` in `ThenReturn` maps to the zero value of the corresponding result
type (a `nil error`, etc.). Later stubs win, like Mockito.

`ThenAnswer` stubs with a closure that sees the call arguments and computes
the results dynamically:

```go
mockito.When(m.Hello("ada")).ThenAnswer(func(args []any) []any {
    return []any{"hi " + args[0].(string)}
})
```

`ThenPanic` makes a call panic — Go's notion of "throwing". To return an
error, just use `ThenReturn` (it accepts any value, errors included):

```go
mockito.When(m.Hello("ada")).ThenPanic("boom")
mockito.When2(m.Greet("ada")).ThenReturn("", errors.New("boom"))
```


## Verification

```go
mockito.Verify(m.Hello("ada")).Once()
mockito.Verify(m.Hello("ada")).Times(2)
mockito.Verify(m.Hello("ada")).AtLeast(2)
mockito.Verify(m.Hello("ada")).AtMost(3)
mockito.Verify(m.Hello("ada")).Between(2, 3)
mockito.Verify(m.Hello("nobody")).Never()
n := mockito.Verify(m.Hello("ada")).Count()
```

`Verify2` / `Verify3` / `Verify4` cover methods with 2–4 results.

## Call order

`NewInOrder` verifies the relative order of calls, advancing a cursor on each
assertion:

```go
io := mockito.NewInOrder()
io.Verify(m.First("x")).Once()   // First("x") happened before Second("y")
io.Verify(m.Second("y")).Once()
```

## Why not gomock?

- **Zero codegen.** No `mockgen`, no generated files to maintain, no
  re-generation when the interface changes.
- **Spies.** `Spy[T](target)` overrides only the methods you stub; gomock
  makes partial mocks awkward.
- **Third-party interfaces.** You only need the interface type, not its
  source — gomock's generator wants the source.
- **Runtime behavior.** Behavior is an interceptor, so it can change by call
  count, arguments, or mid-test, instead of being fixed in generated code.

## How it works

The proxy is go-weave's forged interface value. Each intercepted call records
itself and looks up stubs by the method's trampoline **code pointer**
(`c.Method.CodePtr()`), not by method name — an exact, string-free match. The
recording style needs one last-call per goroutine, which `internal/gls`
provides by using the g pointer itself as the goroutine-local key — no struct
mirroring, no version splitting.

## Limitations

- Recording-style stubbing matches arguments by exact value
  (`reflect.DeepEqual`); there is no "any argument" matcher yet.
- Go 1.18+ on amd64/arm64 — the platforms go-weave supports.
