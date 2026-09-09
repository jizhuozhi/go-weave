# Dispatch & the call path

The trampoline shuffles the registers into `Dispatch`, which is the entry point
of the runtime logic: translate the raw registers into Go values the
interceptors can operate on, run the interceptor chain, then write the results
back to the registers. This document explains the two ends of this path — how
the entry receives, and how the tail returns.

## Entry: how the registers come in

`Dispatch`'s signature is the architecture's "maximal register file"
(`dispatch_arm64.go` / `dispatch_amd64.go`):

```go
func Dispatch(idx int, a0, ..., a15 uintptr, f0, ..., f15 float64, stack unsafe.Pointer) (...)
```

Matching the shuffle the trampoline performs (see the machine-code walkthrough
in [05-jit.md](05-jit.md)):

- `idx` is hardcoded into R0 by the trampoline (`MOVZ $idx, R0`);
- `a0` = the original R0 = the **receiver** (an interface call always puts the
  receiver in the first integer register);
- the rest `a1..a15` are the original R1..R15 shifted along;
- `stack` = `&s0`, the caller's stack-argument-area pointer (written by the
  trampoline into a spill slot).

`idx` is the method's index in the interface, which is also the index into
`itab.Fun`. `Dispatch` uses it to resolve the concrete `*Method` from the
receiver's proxy — so **one slot serves method k of every interface**, and the
slot count is limited only by the per-interface method count.

## Register placement and GC safety

`Dispatch` first writes the registers into the pooled `callState` (the `regBuf`
of `materialize.go`):

```go
type regBuf struct {
    ints   [intArgRegs]uintptr        // raw bit patterns, not scanned by GC
    ptrs   [intArgRegs]unsafe.Pointer // only the pointer-holding registers, GC scans these
    floats [floatArgRegs]float64
}
```

`ints` is `uintptr`, not scanned by the GC; `ptrs` holds only the registers
`layout.ptrMask` marks as pointers (the `prePtrs` loop in `Dispatch`). This split
means the GC neither misses pointer arguments nor aborts on an ordinary integer
that happens to look like a heap address. The `uintptr` vs `unsafe.Pointer`
distinction is in [03-runtime.md](03-runtime.md).

Stack arguments work the same way: `Dispatch` copies `stackCallArgsSize` bytes
from `&s0` into the pooled `stackBuf`, so a stack move inside an interceptor
leaves no dangling pointer.

## The two paths at the tail

`Invocation.Proceed` (`invocation.go`) runs the interceptor chain and then takes
one of two paths to the target, depending on whether arguments were
materialised:

**fast path (`redial`)** — `c.args == nil && stackBytes == 0`:

```go
c.regs.ints[0] = uintptr(m.targetData)  // only swap the receiver
redial(m.targetFun, c.regs)             // replay the registers as-is to the target
c.direct = true
return nil
```

`redial_*.s` reloads the registers from `regs`, `CALL`s the target's bare code
pointer, and stores the result registers back. **No reflect, no boxing, zero
allocation** — this is why `ProxyAdd` reaches 47 ns / 0 allocs.

**reflect fallback** — the interceptor accessed the arguments (`Args()` lazily
triggers `materializeArgs`), or there are stack arguments:

```go
return m.targetFn.Call(c.Args())  // or CallSlice (variadic)
```

Once arguments are materialised into `[]reflect.Value`, the call goes through
`reflect.Value.Call`, an order of magnitude slower (about 265 ns / 6 allocs).

## materialize and scatter

`materialize.go` is the "registers ↔ reflect.Value" codec layer:

- **`materializeArgs`**: follows `abiLayout`'s step sequence to restore the raw
  register/stack bit patterns into `[]reflect.Value`. A single-register or
  single-stack-slot argument is described in place with
  `reflect.NewAt(...).Elem()` (zero copy); one spread across several registers
  (string/slice/small struct) is first copied into a gather buffer and then
  described.
- **`scatterValue` / `storeResults`**: the reverse direction, writing results
  back to registers or stack slots. string/slice/complex have dedicated split
  paths to avoid allocation.

## One complete call chain

```
interface call x.M(args)
  → CALL (R6)                     // itab.Fun[k], a bare code pointer; args already in registers/stack
  → trampoline (jitcode)          // shuffle: idx into R0, receiver shifted right, &s0 into a spill slot
  → Dispatch                      // registers into regBuf, stack args into stackBuf
  → Invocation.Proceed            // run the interceptor chain
      ├─ fast path: redial        // registers straight to the target, zero reflect
      └─ fallback: reflect.Call   // args materialised, then reflection
  → storeResults                  // results scattered back to registers/stack
  → RET                           // results returned to the caller as-is
```
