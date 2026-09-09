# Trampolines & JIT

`itab.Fun[k]` needs a bare code pointer, and this library generates it at
runtime: a piece of machine code (the trampoline) plus a self-consistent
`moduledata` (so the runtime recognises the machine code). This document walks
through how both are constructed field by field. The pclntable format is in
[04-pclntab.md](04-pclntab.md).

## Two kinds of trampoline

| trampoline | generated when | purpose |
|---|---|---|
| **generic** (128 slots) | prefetched at startup `init` | methods with no pointer stack arguments; one slot serves method k of every interface |
| **precise** | per shape at proxy construction, cached | methods with pointers through the stack argument area |

The machine code of the two is nearly identical; the only difference is the
pointer map in `moduledata`: the generic trampoline declares the argument area
pointer-free (`argPtrs = 0`), while the precise one declares it word by word
(`argPtrs`/`retPtrs` mark every pointer-holding word).

## Machine code, instruction by instruction

`jitcode_arm64.go`'s `jitStubCode` parses AArch64 GNU-syntax strings into machine
code via `asm`, so the trampoline body reads like assembly (amd64 uses Intel
syntax, same mechanism):

```go
put(asm("SUB R20, SP, #%d", jitFrameSize)) // compute new SP into R20 (288 = jitFrameSize)
put(asm("STP R29, R30, [R20, #-8]"))       // save old FP, LR to the new frame
put(asm("ADD SP, R20, #0"))                // MOV SP, R20 (ADD #0: ORR's r31 is ZR, not SP)
put(asm("SUB R29, SP, #8"))                // set this frame's frame pointer
```

`asm` is a lightweight assembler: `tokenize` splits on delimiters, `parseReg`/
`parseImm` parse the operands, `parseIns` dispatches by mnemonic to the encoding
helpers (`subImm`/`strOff`/`movz`…). Each helper just does bit assembly, with the
field layout spelled out in comments. It only recognises the instruction subset
`jitStubCode` uses, so a mistyped instruction panics at `init` pre-generation
time.

The first four instructions are the standard function prologue, identical to
what the compiler emits. The key part is that it has **no `morestack` check**
(`//go:nosplit` semantics); its frame size is fixed, so `pcsp` can be encoded
precisely over the prologue/epilogue's SP-change ranges (see `encodePCSP` in
[04-pclntab.md](04-pclntab.md)).

```go
put(asm("STR R15, [SP, #8]"))                // spill the 16th integer register to a stack slot
put(asm("ADD R16, SP, #%d", jitFrameSize+8)) // compute &s0 (start of the caller's stack argument area)
put(asm("STR R16, [SP, #16]"))               // &s0 into the next spill slot
```

`Dispatch`'s signature is `(idx int, a0..a15 uintptr, f0..f15 float64, stack
unsafe.Pointer)` — `idx` + 16 integers + 1 stack pointer, exceeding the 16
integer registers, so the 17th (`a15`) and 18th (`stack`) spill to the stack.
These two `STR`s fill those two spill slots.

```go
for r := 14; r >= 0; r-- {
    put(asm("MOV R%d, R%d", r+1, r)) // shift all integer registers right by one
}
put(asm("MOVZ R0, #%d", sh.index))   // free R0 for the slot index
```

Why shift right: at an interface call the receiver is in R0, and `Dispatch`'s
`a0` parameter is also agreed to land in R1. Moving R0→R1, R1→R2, …, R14→R15
frees R0 for `idx`, so inside `Dispatch`, `ints[0] == a0 == receiver`.

```go
put(asm("MOVZ R16, #%d", uint16(dispatch)))        // load the 64-bit absolute address in pieces
put(asm("MOVK R16, #%d, LSL #16", uint16(dispatch>>16)))
put(asm("MOVK R16, #%d, LSL #32", uint16(dispatch>>32)))
put(asm("MOVK R16, #%d, LSL #48", uint16(dispatch>>48)))
put(asm("BLR R16"))                                // jump to Dispatch
```

arm64's `BL` only reaches ±128 MB, and the distance between the mmap'd page and
the text segment can exceed that, so `MOVZ`/`MOVK` load `Dispatch`'s 64-bit
address straight into R16 and `BLR` through it.

```go
put(asm("LDP R29, R30, [SP, #-8]")) // restore FP, LR
put(asm("ADD SP, SP, #%d", jitFrameSize))
put(asm("RET"))
```

Restore the frame pointer, link register, and SP before returning.

The precise trampoline additionally **clears the result words marked as
pointers** before the `BLR` (`STR ZR, [R16, #w*8]`): those words still hold the
caller's old frame contents while the new argument map already claims they are
pointers, so a GC before the first safe point would read stale data. The
trampoline is pure machine code with no safe point before the `BLR`, so clearing
them first suffices.

## moduledata, field by field

`buildJITModule` fills in every table findfunc/GC needs, following the format in
[04-pclntab.md](04-pclntab.md):

| field | filled with | the runtime read that consumes it |
|---|---|---|
| `pcHeader` | `magic=0xfffffff1`, `minLC=PCQuantum`, `ptrSize=8`, `nfunc=1` | `moduledataverify1` checks magic/minLC/ptrSize |
| `funcnametab` | `"\0weave.jitstub\0"` | `Func.Name()` reads the name |
| `pctab` | `[0]` sentinel + the spdelta range table from `encodePCSP` | `pcvalue` decodes pcsp |
| `pclntable` | one `_func` (with the pcdata/funcdata tail arrays) | `findfunc` locates, `getStackMap` reads bitmaps |
| `ftab` | two rows: `{0,0}` + trailing sentinel | `findfunc`'s bucket-search convergence |
| `findfunctab` | one bucket, `idx=0`, subbuckets all 0 | `findfunc` steps 3, 4 |
| `minpc`/`maxpc` | `text` / `etext` | `findmoduledatap`'s range match |
| `gofunc` | inlined args map + locals map | `funcdata` returns `gofunc+off`, dereferenced directly |

`registerModule` links the module onto the tail of `runtime.lastmoduledatap` via
`//go:linkname`, so `findfunc` can reach it. Once registered, the page and every
allocation it references must live until process exit, so they are all kept in
`jitRoots` permanently.

## Version-split mirrors

`moduledata` and `_func` field order changes across Go versions, and the mirrors
are split into segments:

| struct | version segments |
|---|---|
| `moduledata` | 1.18/1.19 baseline; 1.20 +`covctrs`; 1.21 +`inittasks`; 1.23 `bad` moves after `hasmain`; 1.26 +`epclntab`; 1.27 drops `typelinks`/`itablinks`, +`typedesclen`/`itaboffset`/`itabsize` |
| `_func` (`rfunc`) | 1.18/1.19 without `startLine`; 1.20+ with `startLine` |

Each segment carries the expected offsets (`text`/`pctab`/`gofunc`/`next`)
checked by `TestJITFindfunc`, and CI runs every version from 1.18 to 1.27, so a
misfilled segment is caught immediately.

## Platform shim: MAP_JIT

On Apple Silicon, `MAP_JIT` pages default to RX; before writing code you must
switch to RW with `pthread_jit_write_protect_np`, switch back to RX afterwards,
and flush the instruction cache with `sys_icache_invalidate`. These C calls are
isolated in `internal/rt` (`jitmem_darwin_arm64.go`, called through
`jit_darwin.go`); other platforms use a plain RWX `mmap` (`jitmem.go`).
