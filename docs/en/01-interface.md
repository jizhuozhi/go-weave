# Interface calls & itab

The core move of this library is forging an `itab` at runtime so that `*Proxy`,
a type that never declared "I implement `T`", becomes a legal interface value
`T`. This document explains the interface value, the `itab` layout, and why the
method table must hold bare code pointers. It is the starting point of the whole
mechanism.

## An interface value = two words

A non-empty interface value is two machine words in memory (`rt.go`):

```go
type iface struct {
    tab  *itab          // dispatch table
    data unsafe.Pointer // underlying concrete value
}

type eface struct {     // empty interface any
    typ  *abiType
    data unsafe.Pointer
}
```

Note that the first word of a non-empty interface is `*itab`, while the first
word of `any` is `*abiType`. The two cannot be mixed — this is also why an
interface value must not be assembled by routing it through an `any` parameter
(the comment on `makeIface` in `rt.go` spells this out: Go converts `*itab` to
`*abiType` on entry into an `any` parameter, destroying the dispatch table).

> **Concurrency aside**: the two words `itab + data` are the proverbial fat
> pointer. Because an interface value is two words, an assignment needs two
> stores, and the compiler does not fuse them into one atomic 16-byte write —
> so interface assignment is non-atomic. Two goroutines reading and writing the
> same interface value concurrently can observe a `tab` and `data` that do not
> belong to the same instant.

## The itab layout

```go
type itab struct {
    Inter *interfaceType // interface type
    Type  *abiType       // concrete type
    Hash  uint32         // a copy of Type.Hash, used by type switches
    _     [4]byte
    Fun   [1]uintptr     // actually variable-length: one bare code pointer per method
}
```

`Fun` is declared with length 1, but the real `itab` is followed by `n` more
`uintptr`s, each the entry code pointer of one method, ordered by interface
method index (consistent with `reflect.Type.Method(i)`).

## Interface dispatch: why it must be a bare code pointer

The machine code the compiler emits for an interface call `x.M()` (roughly, on
arm64):

```text
MOVD 24(itab), R6   // load the code pointer of Fun[k]
CALL (R6)           // indirect call, no closure context passed
```

`24` is the byte offset of the `Fun` array inside `itab`: `Inter` (8) + `Type`
(8) + `Hash` (4) + padding (4) = 24. The method index `k` is known at compile
time, so the pointer is loaded from `24 + 8*k`.

The key constraint is that **`CALL (R6)` carries no closure context**, which
rules out two candidates:

- **closures**: a closure entry expects to read its context from the closure
  register, but the interface call provides none;
- **`reflect.MakeFunc`**: its entry `makeFuncStub` likewise expects a context in
  the closure register.

So `Fun[k]` can only be a bare code pointer — an ordinary function, or
runtime-generated machine code (this library uses the latter, see
[05-jit.md](05-jit.md)).

## Forging the itab

`forgeITab` (`rt.go`) builds the itab's three header words and n method pointers
inside a `[]unsafe.Pointer`:

```go
func forgeITab(inter *interfaceType, proxyType *abiType, funs []unsafe.Pointer) *itab {
    n := len(inter.Methods)
    w := make([]unsafe.Pointer, 3+n)
    w[0] = unsafe.Pointer(inter)
    w[1] = unsafe.Pointer(proxyType)
    *(*uint32)(unsafe.Pointer(&w[2])) = proxyType.Hash // Hash shares a word with the 4-byte padding
    for i, f := range funs {
        w[3+i] = f
    }
    return (*itab)(unsafe.Pointer(&w[0]))
}
```

**Why `[]unsafe.Pointer` and not raw memory**: the runtime allocates real itabs
with `persistentalloc` (never reclaimed, never scanned by the GC), but a forged
itab's `Inter`/`Type`/`Fun` are all references (method stubs, type descriptors).
Putting them in a `[]unsafe.Pointer` lets the GC scan those references, so they
are never reclaimed.

## Assembling the interface value

`makeIface` (`rt.go`) writes the forged itab and the data pointer directly into
the storage of a `*T`:

```go
func makeIface[T any](dst *T, tab *itab, data unsafe.Pointer) {
    i := (*iface)(unsafe.Pointer(dst))
    i.tab = tab
    i.data = data
}
```

In `NewOf`, the `Type` field records `*Proxy` (`typeOf(reflect.TypeOf(p))`),
consistent with the object the data pointer points at, so the GC scans the
`*Proxy`'s fields correctly.

## The init self-check

The `itab` layout has no compatibility promise. `rt.go`'s `init` validates three
critical offsets against a real itab (`&itabProbe{}` assigned to
`itabProbeIface`):

- `itab.Inter` lands on an interface-kind type descriptor (validating the itab
  header and the `interfaceType` embedding);
- `itab.Hash == itab.Type.Hash` (validating the two `Hash` fields);
- `itab.Fun[0]` points at the method's own entry (validating the `Fun` offset).

Any mismatch panics with an unsupported-Go-version message, rather than
silently corrupting memory at runtime.
