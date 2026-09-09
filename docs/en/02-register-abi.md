# The register ABI

The trampoline is a bare code pointer with a "maximal register signature". To
translate the received arguments into Go values and write the results back, we
must replicate Go's register-assignment algorithm. This document explains how
arguments are split between registers and the stack. It is the foundation for
`materialize`/`scatter` later on.

## Why this algorithm is needed

When an interface call happens, the arguments are already in place per Go's ABI:
integers and pointers in integer registers (R0–R15 on arm64, AX/BX/CX/DI/SI/R8–R11
on amd64), floats in float registers, and whatever does not fit falls into the
caller's stack argument area. After the trampoline receives them as-is,
`Dispatch` needs to know "where is argument i" in order to:

- `materialize` the argument into a `reflect.Value` for the interceptor;
- `scatter` the interceptor's rewritten results back into the right
  register/stack position.

`abi.go` is a private port of the register-assignment part of `reflect/abi.go`
(reflect's implementation is unexported), built on `reflect.Type`.

## The algorithm: a sequence of "steps"

Every argument is expanded into several `step`s, each one a transfer
instruction:

```go
type step struct {
    kind   stepKind // stepStack / stepIntReg / stepPointer / stepFloatReg
    offset uintptr  // offset inside the Go value
    size   uintptr  // byte count of this step
    stkOff uintptr  // offset in the stack argument area (stepStack)
    ireg   int      // integer register index
    freg   int      // float register index
}
```

Rules (`regAssign`):

- pointer/chan/map/func → 1 integer register, marked `stepPointer`;
- integer/bool → 1 integer register;
- float → 1 float register (complex takes 2);
- `string` → 2 integer registers (data is a pointer, len is not), pointer bitmap
  `0b01`;
- `interface` → 2 integer registers (itab and data are both pointers), `0b10`;
- `slice` → 3 integer registers (ptr/len/cap), `0b001`;
- `struct` → recurse field by field; if any field fails to fit, the whole struct
  fails;
- **once an argument fails to fit the remaining registers, it spills to the
  stack in its entirety, and every later argument stays on the stack too** (Go's
  "spill-to-stack means the whole argument" rule).

## The stack argument area and home slots

Arguments that do not fit the registers go into the caller's **outgoing area**
(the stack argument area). `abiLayout` records a few key quantities:

- `stackCallArgsSize`: the byte count of the stack arguments (`Dispatch` copies
  these bytes from `&s0` into a pooled buffer);
- `retOffset`: the start of the stack results (does not share space with the
  arguments, aligned to `ptrSize`);
- `stackBytes`: the whole argument area (arguments and results).

One concept runs throughout: the **home slot**. A register argument also has a
"home" in the caller's frame (reserved per `_func.args`), where morestack saves
the register when the stack grows. An interface call site reserves home slots
per the **method's own signature**, while the trampoline's signature is "the
maximal register file" — the two home-slot sizes differ, which is why the
trampoline must have no stack check (`//go:nosplit`); see the morestack part of
[03-runtime.md](03-runtime.md).

## Pointer bitmaps

Two pieces of pointer information are critical for GC safety:

- **`ptrMask`** (integer registers): bit i says whether the i-th integer
  argument register holds a pointer. `Dispatch` copies only the pointer-holding
  registers into the GC-visible `ptrs` mirror, leaving the rest in `uintptr`
  unscanned — otherwise an ordinary integer that happens to look like a heap
  address would be mistaken for a pointer and trigger "found bad pointer in Go
  heap".
- **`stackPtrOffs` / `retStackPtrOffs`** (stack argument area): the byte offset
  of each pointer among the stack arguments/results. They decide whether the
  method needs a "precise trampoline"; see [05-jit.md](05-jit.md).

## A complete layout

`newABILayout` computes everything for a method type: first `addRcvr` (the
receiver always takes the first integer register, a pointer bit), then `addArg`
for each argument, and finally the results. The resulting `abiLayout` is the
entire input to `materialize`/`scatter`/`shape`.
