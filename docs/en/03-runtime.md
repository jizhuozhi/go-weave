# Runtime stacks & GC

This library leans heavily on the Go runtime's low-level behavior: register
spills must be visible to the GC, stack moves must not leave dangling pointers,
and `uintptr` vs `unsafe.Pointer` is a real choice. This document unpacks those
mechanisms and explains the "how", not just the "what".

## GMP and the goroutine stack

- **G** (goroutine): a userspace lightweight thread with its own call stack;
- **M** (machine): the OS thread that actually executes code;
- **P** (processor): a logical processor holding a local runq and scheduling
  context, deciding which G runs on which M.

The stack is part of the G: each goroutine has one contiguous region of memory,
initially ~2 KB, growing on demand, capped at 1 GB (`runtime.maxstacksize`). A
stack can **split** and can **move** — the two mechanisms below.

## Stack splitting: morestack

Every function prologue has a stack check — comparing SP against
`g.stackguard0`, jumping to `runtime.morestack` if SP crosses the guard:

```
function prologue (compiler-generated):
    CMP SP, stackguard
    BLO morestack        // stack is exhausted
    ... normal execution
```

`morestack` saves the state and enters `runtime.newstack`, which allocates a
larger stack and triggers `copystack`. This library's JIT trampoline is pure
machine code with **no such check** (the compile-time `//go:nosplit` equivalent);
its frame size is fixed, so the `pcsp` table can describe the stack-pointer
offset at every PC exactly, which is what lets traceback unwind correctly — this
corresponds to the pcvalue section of [04-pclntab.md](04-pclntab.md).

## Stack moves: how copystack adjusts pointers

On growth, `copystack` allocates a new stack and copies the old stack's contents
over. The hard part: **every pointer on the old stack now points at the wrong
place**, and all of them must be rewritten to the new addresses:

```go
func copystack(gp *g, newsize uintptr) {
    old := gp.stack
    new := stackalloc(newsize)          // 1. allocate the new stack
    memmove(new, old, used)             // 2. copy the contents
    adjinfo := adjustinfo{old, new}
    gentraceback(..., adjustframe, ...) // 3. walk every frame, adjust pointers
    ...
}
```

`adjustframe` (`runtime/stack.go`) for each frame:

```go
locals, args, objs := frame.getStackMap(true)  // get this frame's pointer bitmap
adjustpointers(frame.varp-size, &locals, ...)  // adjust pointers in locals
adjustpointers(frame.argp, &args, ...)         // adjust pointers in the argument area
```

`adjustpointers` walks the bitmap word by word: a word with bit 1 is a pointer,
subtract `old` and add `new` to get the new address; a word with bit 0 is
skipped.

**The key trap follows from this**: only values the bitmap marks as pointers get
adjusted. A `uintptr` on the stack holds a stack address's bit pattern, the
bitmap does not consider it a pointer, `adjustpointers` will not touch it, and
after a stack move it becomes a dangling address.

This is exactly why this library keeps the call state on the heap — the
`callState` comment in `materialize.go`:

> *Keeping all of it off the goroutine stack also makes it immune to stack
> moves — a stack growth inside an interceptor would leave stack-allocated state
> dangling.*

The stack argument area's raw pointer (`&s0`) never enters a heap object:
`Dispatch` copies it into the pooled `stackBuf`, so even if the stack grows
inside an interceptor, this copy stays stably on the heap.

## GC and stack scanning

The GC is tri-color marking and the **stack is a root**. In the mark phase,
starting from the roots, `gentraceback` walks each frame during a stack scan,
calls `getStackMap` for each frame to obtain the locals/args bitvectors, and
decides word by word: a word with bit 1 is a pointer to trace (a new marking
source); a word with bit 0 is treated as an integer and skipped.

These bitvectors come from pclntable's `stackmap` (see
[04-pclntab.md](04-pclntab.md)). **A bare mmap'd page is in no moduledata**, so
`findfunc` cannot find its `_func`, `getStackMap` returns empty, and the GC
crashes with `missing stackmap`. This is precisely why JIT must forge a
moduledata.

The write barrier is the other piece: during concurrent marking, pointer writes
must be recorded through the write barrier so no mark is lost. Its effect on
this library is indirect — as long as pointers are placed into `unsafe.Pointer`
fields correctly (next section), the write barrier handles it for us.

The `GODEBUG=clobberfree=1` test verifies this whole chain: reclaimed memory is
filled with a sentinel value, so any "missed pointer" (a stack word that should
have been marked but was not) immediately reads the sentinel and crashes.

## unsafe.Pointer vs uintptr

| | semantics | GC marks | stack move |
|---|---|---|---|
| `unsafe.Pointer` | a pointer, holdable across GC | traced | adjusted |
| `uintptr` | an integer, just an address's bit pattern | **not traced** | **not adjusted** |

Corollaries:

- Once a pointer is converted to `uintptr`, as long as the original pointer is no
  longer referenced, the GC may reclaim the object and the `uintptr` becomes
  dangling. So `uintptr` is **transient**: `p := unsafe.Pointer(uintptr(x) + off)`
  must convert back to a pointer within the same expression; holding a `uintptr`
  across a statement or a GC is a bug.
- When you deliberately want to "downgrade a pointer to an untraced integer"
  (e.g. to keep the GC from mistaking an integer that resembles a heap address),
  `uintptr` is the intended tool.

Two applications in this library:

1. **the `regBuf` dual mirror** (`materialize.go`): `ints [16]uintptr` holds the
   raw register bit patterns (not scanned), and `ptrs [16]unsafe.Pointer` is
   filled only with the registers `ptrMask` marks as pointers (scanned). Without
   the former, the GC would mistake ordinary integers for pointers and trigger
   "found bad pointer in Go heap"; without the latter, it would miss real
   pointer arguments.

2. **before `Dispatch` returns** (`dispatch_*.go`): the results are
   `unsafe.Pointer`s converted back from `uintptr`, spilled within this frame;
   the comment states explicitly *"No defer on the Put, and no call after this
   point"* — guaranteeing no GC opportunity observes these half-finished
   conversions between the conversion and the return.
