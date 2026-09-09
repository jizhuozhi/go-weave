# go-weave Technical Whitepaper

The whole point of this project is to prove Go can do AOP (aspect-oriented
programming) through dynamic proxies, the way Java does.

This document set is written so that a reader who only writes ordinary Go code
can finish it and **fully understand why go-weave works** — not through
undocumented runtime black magic, but through the **systematic use of
mechanisms Go has publicly committed to (or that are de-facto stable)**.

go-weave does something that looks impossible: it forges an `itab` at runtime
so that `*Proxy`, a type that never declared "I implement `T`", becomes a legal
interface value. It patches no bytecode and hooks no runtime internals. It just:

1. Understands that an interface value is two words, `(itab, data)`, and builds
   its own `itab`;
2. Understands that the interface call `CALL (R6)` needs a bare code pointer,
   so it JIT-generates a piece of machine code at runtime;
3. Understands that the GC scans stacks via the pointer maps in pclntable, so it
   forges a self-consistent `moduledata` the GC can recognise;
4. Understands the ABI layout of arguments across registers/stack, so it
   re-implements the register-assignment algorithm to translate them into Go
   values.

Every step is backed by a corresponding runtime mechanism. The documents are
organised along this dependency chain, building up from the low-level concepts.

Every technique this library uses is either a mechanism Go's distribution makes
public (the two-word interface layout, the register ABI, the public pclntable
format), or one of a small number of `//go:linkname` symbols pinned onto
unexported runtime layouts — the latter are exactly the use the Go source's
"hall of shame" comments call out, alongside sonic; their long-term stability is
discussed by rsc in GitHub issues (e.g. go.dev/issue/67401, 71672).

## Reading order

Read in numbered order; each document depends only on the concepts before it:

1. **[01-interface.md](01-interface.md) — Interface calls & itab**: the
   two-word interface layout (fat pointer), the itab structure, and why the
   method table must hold bare code pointers. This is where the whole problem
   starts.

2. **[02-register-abi.md](02-register-abi.md) — The register ABI**: how
   arguments are split between registers and the stack, home slots, the outgoing
   area, and pointer bitmaps. Answers "where is the argument, and which word is a
   pointer".

3. **[03-runtime.md](03-runtime.md) — Runtime stacks & GC**: GMP, stack
   splitting, stack moves (how copystack adjusts pointers), tri-color marking,
   and how stack scanning decides what is a pointer from the pointer map. The
   foundation for "why pointers must be visible to the GC".

4. **[04-pclntab.md](04-pclntab.md) — pclntable, white-boxed**: how an
   executable describes itself — pcHeader, `_func`, pcvalue's varint encoding,
   funcdata, stackmap, findfunc's bucket search. The prerequisite for "why JIT
   must forge a moduledata".

5. **[05-jit.md](05-jit.md) — Trampolines & JIT**: the machine-code layout,
   how moduledata maps field-by-field onto runtime reads, the two trampoline
   kinds (generic/precise), version segments, and the MAP_JIT platform shim.

6. **[06-dispatch.md](06-dispatch.md) — Dispatch & the call path**: how the
   registers enter Dispatch, how the interceptor chain runs, and the fast path
   vs reflect fallback, materialize/scatter.

## Concept map

```
       01 Interface value (itab, data)
              │  Fun[k] must be a bare code pointer
              ▼
       05 JIT-generated machine code + forged moduledata
          │                          │
          │ how arguments travel      │ how the GC recognises this code
          ▼                          ▼
   02 Register ABI              04 pclntable (findfunc → pointer map)
          │                          │
          └──────────┬───────────────┘
                     ▼
              03 Runtime stack & GC (stack scan, moves, pointer decisions)
                     │
                     ▼
              06 Dispatch (translate + interceptor chain + writeback)
```

The core loop is: **an interface call jumps in through a bare pointer → JIT
builds code findfunc can recognise → the GC knows which stack words are pointers
from pclntable's maps → arguments are translated per the ABI into Go values that
flow through the interceptor chain**. Every link has a matching runtime
mechanism; none of it is luck.

## Glossary

| Term | Meaning | See |
|---|---|---|
| interface value / `iface` / `eface` | two machine words: `(itab, data)` or `(_type, data)` | 01 |
| fat pointer | a data pointer that carries type metadata | 01 |
| itab / `Fun[k]` | the interface dispatch table, an array of method entry code pointers | 01 |
| bare code pointer | a function entry address with no closure context | 01 |
| register ABI / home slot | Go's calling convention; the register argument's "home" in the caller's frame | 02 |
| outgoing area / stack argument area | the region the caller reserves for the callee's stack arguments | 02 |
| pointer bitmap / `ptrMask` | marks which registers/stack words are pointers | 02 |
| GMP | goroutine / OS thread / logical processor | 03 |
| stack splitting / `morestack` | the growth check in a function prologue | 03 |
| stack move / `copystack` | copying a stack and adjusting all its pointers | 03 |
| tri-color marking / write barrier | the GC's marking algorithm and its incremental safety mechanism | 03 |
| pointer map / `stackmap` / bitvector | a bitmap describing which stack words are pointers | 03, 04 |
| pclntable / `pcHeader` / `_func` | the tables describing function metadata in an executable | 04 |
| pcdata / funcdata / pcvalue | per-function metadata and PC-encoded tables | 04 |
| `findfunc` / `findfuncbucket` / `ftab` | the lookup chain from a PC to function metadata | 04 |
| moduledata | the metadata collection of one loadable module | 04, 05 |
| MAP_JIT / write-protect | the write protocol for executable pages on Apple Silicon | 05 |
| `uintptr` vs `unsafe.Pointer` | the former is not traced by the GC, the latter is | 03 |
