# weave — runtime dynamic proxies for Go

```go
import "github.com/jizhuozhi/go-weave" // package weave

p := weave.New[UserService](realService,
    func(c *weave.Invocation) []reflect.Value {
        start := time.Now()
        rets := c.Proceed()
        log.Printf("%s took %v", c.Method.Name, time.Since(start))
        return rets
    },
)
p.Load(ctx, 42) // goes through the interceptor
```

`weave.New[T]` returns a value of the interface type `T` that behaves like the
target but runs your interceptors around every method call — Go's answer to
`java.lang.reflect.Proxy`, built on the observation that an interface value is
just `(itab, data)` and nothing stops you from forging the itab yourself.

Supports **arm64** and **amd64**, verified on every Go minor version from
**1.18** through **1.24** (the register ABI's debut on both architectures).
The runtime layouts the package forges are validated at init against a real
itab, so an unsupported future Go fails at startup with a clear panic instead
of corrupting memory.

## What you get

| Feature | Notes |
| --- | --- |
| Around advice | chain of `Interceptor` funcs, run in order |
| Argument rewriting | `c.SetArg(i, v)` before `c.Proceed()` |
| Result rewriting | return different values from an interceptor |
| Short circuit | skip `Proceed()` entirely |
| Mocking | `weave.New[T](nil)` returns zero values for every method |
| Declarative APIs | interface as the only declaration, interceptor as the implementation |
| Reflection-free hot path | 47 ns / 0 allocs per call, `-race` supported |

## Declarative DAOs

The Java ecosystem's MyBatis-style mapper — an interface that *is* the data
access layer, no implementation struct, no codegen — becomes a plain Go
pattern:

```go
type UserDAO interface {
    GetUser(ctx context.Context, id int64) *User
    ListUsers(ctx context.Context, limit int) []string
}

var queries = map[string]string{
    "GetUser":   "SELECT id, name FROM users WHERE id = ?",
    "ListUsers": "SELECT name FROM users LIMIT ?",
}

dao := weave.New[UserDAO](nil, executor) // executor maps method → SQL → result
u := dao.GetUser(ctx, 2)               // a real *User, via a real query plan
```

The interceptor inspects `c.Method.Name`, binds `c.Args()` to the query and
produces the results — the same role MyBatis' XML mapping plays, minus the
annotation processing and codegen. A runnable version of this example is in
[`example_test.go`](example_test.go); `go test` executes it. The same shape
gives you RPC stubs (interface = the wire contract), middleware for arbitrary
business interfaces, and runtime mocks without gomock's generation step.

## How it works

### The itab forgery

A Go interface value is `iface{tab *itab, data unsafe.Pointer}`, and the
compiler lowers `x.M()` to an indirect call through `itab.Fun[k]`. The runtime
only builds itabs for types that statically implement the interface — so this
package simply allocates one itself: `Inter` points at the real interface type,
`Type` at `*Proxy`, and each `Fun[k]` at a trampoline for that method. The
resulting value is a first-class `T` end to end; it can be stored, passed and
called like any other `T`.

### The trampolines

`Fun[k]` must be a **bare code pointer** — the dispatch sequence passes no
closure context. That rules out closures (their entry expects a context in the
closure register) and rules out `reflect.MakeFunc` (whose entry `makeFuncStub`
expects the same).

So the trampolines are ordinary Go functions, generated **at build time** by
[`gen/main.go`](gen/main.go) into `stubs_gen_arm64.go` / `stubs_gen_amd64.go`.
Each of the 128 slots is a separate function with the architecture's maximal
register signature, one per **interface method index**:

```go
//go:nosplit
//go:norace
func stub0(a0, a1, ... a15 uintptr, f0, ... f15 float64) (r0, ... r15 unsafe.Pointer, g0, ... g15 float64) {
	return Dispatch(0, a0, a1, ... a15, f0, ... f15)
}
```

Separate functions give separate code pointers (no closure context needed —
exactly what `itab.Fun[k]` expects). The slot index doubles as the method's
index in the interface, and the receiver identifies the proxy, so one slot
serves method k of **every** interface simultaneously — the number of
interfaces and proxies is unbounded.

`dispatch` looks the method up through the receiver, decodes the registers
with a private copy of Go's register-assignment algorithm ([abi.go](abi.go)),
runs the interceptor chain and scatters the results back into the same
registers. Arguments living in a single register are described in place with
`reflect.NewAt` — zero copy, zero boxing — and only multi-register arguments
(strings, slices, small structs) are gathered into a scratch buffer.

### The two directives are load-bearing

A register parameter's ABI **home slot** lives in the *caller's* outgoing
argument area. A direct call to the stub's signature would reserve all 32
homes — but an itab call site is compiled against the *method's* signature and
reserves only the method's own homes (often just a couple of words). Both
directives exist to keep the stub from writing past that reservation:

- `//go:norace` — the race prologue spills all parameters across
  `racefuncenter`, straight into the unreserved homes. This was the source of
  a spectacular memory corruption under `-race`.
- `//go:nosplit` — a split function's morestack trampoline saves every
  argument register to its home before growing the stack (`gobuf` doesn't
  cover argument registers), again writing ~250 bytes into the caller's
  frame. This one is not race-specific: any proxy call landing exactly on a
  stack-growth boundary would corrupt the caller.

The stub is a pure register conduit with no memory accesses, so excluding it
from race instrumentation costs nothing — from `dispatch` down, everything
(interceptors, reflect, user code) stays fully instrumented and `-race` is
supported on both architectures.

### Calling the target: `redial`

When no interceptor has materialised the arguments, `callTarget` skips reflect
entirely: a small assembly helper ([`redial_arm64.s`](redial_arm64.s) /
[`redial_amd64.s`](redial_amd64.s)) reloads the original argument registers,
swaps the receiver to the target (both taken from the target's *real* itab,
extracted once at proxy construction), and calls the target's own method code
pointer. The results land directly in the result registers — no boxing, no
frame building, no `reflect.Value.Call`. A register snapshot taken before the
call keeps the arguments observable (`Args()` after `Proceed`) and replayable
(a second `Proceed`), with a GC-visible pointer mirror.

The assembly follows the same shape as the runtime's `reflectcall` wrappers:
the frame is sized to house the callee's register-argument home slots, an
empty locals stack map is declared via `no_pointers_stackmap`, and pointers
the callee spills into its homes are adjusted on stack moves by the callee's
own argument maps.

```sh
name                              time/op     allocs/op
DirectAdd                          0.25 ns          0
ProxyAdd (no interceptor)           47 ns           0
ProxyIntercept (observing)          48 ns           0
ProxyInspect (rewrites args)       273 ns           6
```

### GC safety

This is the part that took the most iterations. The trampoline's integer
parameters carry raw register contents, most of which are garbage for any given
signature:

- typed `unsafe.Pointer` → the compiler marks the spill area as pointers and
  the collector eventually aborts with *"found bad pointer in Go heap"* when a
  garbage word happens to look like an address in an unused span region;
- typed `uintptr` → the collector ignores them, and can then free a pointer
  argument that is still live only in a register.

The fix mirrors `runtime.RegArgs`: parameters are `uintptr` (never scanned) and
the dispatcher's first action is to copy the registers the method's layout
marks as pointer-holding into a parallel `ptrs [N]unsafe.Pointer` array the
collector *does* scan — before the first allocation. Verified with
`GODEBUG=clobberfree=1`.

### Runtime JIT

Methods that move pointers through the caller's stack argument area need a
trampoline whose argument pointer map describes that area word by word. The
generic trampoline declares it as one opaque `[480]byte` — fine for integers, a
lie for pointers — and the lie cannot be corrected from Go, because the
correction would have to happen before the trampoline's first instruction.

So those trampolines are generated **at runtime**, at proxy construction, one per
*shape* of stack area (argument words then result words, each marked pointer or
not). The proxy mmaps an executable page, writes the trampoline's machine code —
the same register shuffle the generic stubs perform — and builds a moduledata
whose argument pointer map marks exactly the pointer words. The module is
appended to the runtime's moduledata list, so `findfunc` recognises the injected
code and the collector scans every pointer in the stack area exactly as it would
for a compiled function. The cost is a one-off per shape, cached; the call site
stays a plain indirect itab call.

Two details are load-bearing:

- the result words still hold the caller's old frame contents while the new map
  already claims they are pointers, so the trampoline **clears them before its
  first call** — it is pure machine code with no safe point until the call, so
  the collector never sees a stale word;
- the forged `moduledata` layout is version-specific, so the JIT path is gated
  to Go 1.23+; earlier releases reject pointer-spilling methods (see
  [Limitations](#limitations)).

Methods whose pointers stay inside the register file — nearly all of them — need
no generated code and keep using the generic trampoline.

## Limitations

- **Stack-passed arguments and results are supported** up to 480 bytes per
  method (arguments or results spilling past the register file, big structs by
  value). When the spilled part carries *pointers*, the stack area's GC
  description belongs to the trampoline, and the generic one describes it as a
  byte window — a lie the collector cannot be talked out of in time. On Go 1.23+
  the trampoline is **generated at runtime** (see
  [Runtime JIT](#runtime-jit)): no codegen step, no shape to spell out. Earlier
  Go releases cannot forge the moduledata layout and reject such a method at
  proxy construction. Since register assignment is positional, moving pointer
  arguments to the front of the signature still avoids the stack area entirely
  (15 integer words remain on arm64 after the receiver, 8 on amd64).
- **Interfaces can have at most 128 methods** — slot k serves method k of every
  interface, so the bound is per-interface, not process-wide. Raise `slots` in
  `gen/main.go` and run `go generate ./...` for more.
- **`any(proxy).(T)` fails.** The proxy passes as `T` end to end, but asserting
  back from an `any` goes through `getitab`, which requires the concrete type
  to statically implement `T`. Use the value directly.
- **Runtime layout dependencies**, mitigated: the forged structures
  (`abi.Type`, interface headers, itab) are validated at init against a real
  runtime itab, so an unsupported Go version fails loudly at startup. CI
  guards every minor version 1.18–1.24 on amd64 and arm64.

## Development

```sh
go generate ./...                    # regenerate the trampoline stubs
go vet -unsafeptr=false ./...        # unsafeptr is off by design
go test ./...                        # arm64 native / amd64 via Rosetta
go test -race ./...                  # supported on both architectures
GODEBUG=clobberfree=1 go test ./...  # GC safety stress
go test -bench . -benchmem ./...
```

CI runs the whole matrix (linux/amd64, linux/arm64, darwin/arm64 — vet, fmt,
tests, race, GC stress) plus a check that `go generate` output is committed
up to date; see [`.github/workflows/ci.yml`](.github/workflows/ci.yml).

## Layout

| File | Role |
| --- | --- |
| `rt.go` | runtime mirrors (`itab`, `eface`), itab forgery, target itab extraction |
| `proxy.go` | public API: `New`, `NewOf`, `As`, `Proxy` |
| `invocation.go` | public API: `Method`, `Interceptor`, `Invocation`, target invocation |
| `abi.go` | private copy of Go's register assignment algorithm |
| `fast.go` | pooled call state, `regBuf`, argument materialisation, result scatter |
| `fast_arm64.go`, `fast_amd64.go` | per-architecture `dispatch` and slot table |
| `redial_arm64.s`, `redial_amd64.s` | register-replay call helper (fast path to target) |
| `tramp.go` | trampoline selection and slot policy |
| `gen/main.go` | trampoline generator (`go generate`) |
| `stubs_gen_*.go` | generated trampolines — do not edit |
| `example_test.go` | runnable declarative-DAO example |
