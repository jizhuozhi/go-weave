# pclntable, white-boxed

To understand "why JIT must forge a moduledata", first understand how a Go
executable **describes itself** — given a program counter (PC), how the runtime
knows which function it belongs to, what metadata the function has, and which
stack words are pointers. This mechanism is called pclntable (program counter
line table).

## In one sentence

pclntable is a set of tables answering two questions:

1. **which function does this PC belong to** (`findfunc`);
2. **what is the stack layout of this function at a given PC**
   (pcdata/funcdata → pointer map).

GC stack scanning, traceback unwinding, and `runtime.Caller` all depend on it.
If an executable segment is not in these tables, the runtime "cannot see" it —
which is why a bare mmap'd page crashes with `missing stackmap`.

## The lookup chain: from PC to function

The full chain of `findfunc` (`runtime/symtab.go`):

```go
func findfunc(pc uintptr) funcInfo {
    datap := findmoduledatap(pc)   // 1. which module
    if datap == nil { return funcInfo{} }

    pcOff, ok := datap.textOff(pc) // 2. PC's offset relative to the module text
    if !ok { return funcInfo{} }

    b := uintptr(pcOff) / abi.FuncTabBucketSize          // 3. which bucket
    i := uintptr(pcOff) % abi.FuncTabBucketSize / (FuncTabBucketSize / nsub) // which subbucket

    ffb := (*findfuncbucket)(add(datap.findfunctab, b*unsafe.Sizeof(findfuncbucket{})))
    idx := ffb.idx + uint32(ffb.subbuckets[i])           // 4. candidate function index

    for datap.ftab[idx+1].entryoff <= pcOff { idx++ }    // 5. linear converge to the real function
    funcoff := datap.ftab[idx].funcoff
    return funcInfo{(*_func)(unsafe.Pointer(&datap.pclntable[funcoff])), datap}
}
```

Each step:

1. **`findmoduledatap`**: walk the `moduledata` linked list (`firstmoduledata →
   next`) to find the module whose `minpc <= pc < maxpc`. JIT's `registerModule`
   appends the forged module to the tail of this list.
2. **`textOff`**: `pc - md.text`, the PC's offset within the module's code
   segment.
3. **`findfuncbucket`**: slice the text into buckets of `FuncTabBucketSize`
   (4096 bytes), each bucket holding 16 subbuckets (each covering 256 bytes). The
   bucket header holds an `idx` (starting function index) plus 16 `subbuckets`
   bytes (each an increment relative to idx). This converges to a small range in
   O(1) instead of binary-searching the whole `ftab`.
4. **`ftab`**: `[]functab{entryoff, funcoff}`, sorted by entryoff. `entryoff` is
   the function entry's offset in text, `funcoff` is the function's `_func`
   offset in `pclntable`.
5. advance linearly until `ftab[idx+1].entryoff > pcOff`; `idx` is the target
   function.

A JIT module has only one function, so `findfunctab` needs only one bucket
(`idx=0`, `subbuckets` all 0) and `ftab` two rows (the function + a trailing
sentinel).

## pcHeader: the module-level header

```go
type pcHeader struct {
    magic          uint32 // 0xfffffff1, the Go 1.20+ PCLnTabMagic
    pad1, pad2     uint8
    minLC          uint8  // minimum instruction length (PCQuantum)
    ptrSize        uint8  // pointer size
    nfunc          int    // function count
    nfiles         uint   // file count
    textStart      uintptr
    funcnameOffset uintptr // offset to funcnametab
    cuOffset       uintptr
    filetabOffset  uintptr
    pctabOffset    uintptr // offset to pctab
    pclnOffset     uintptr // offset to pclntable
}
```

`magic` is the version number; `moduledataverify1` validates it at startup — a
wrong magic crashes with `invalid function symbol table`. `minLC`
(= `PCQuantum`, 4 on arm64, 1 on amd64) is the unit of the PC deltas in pcvalue
tables, used below.

## _func: per-function metadata

Each row of `pclntable` is a `_func` (mirrored as `rfunc` in this library):

```go
type _func struct {
    entryoff   uint32 // entry offset relative to text
    nameoff    int32  // name offset in funcnametab
    args       int32  // argument area size (bytes)
    deferreturn uint32
    pcsp       uint32 // pcsp table offset in pctab
    pcfile     uint32
    pcln       uint32
    npcdata    uint32 // number of pcdata table entries
    cuOffset   uint32
    startLine  int32  // added in Go 1.20
    funcID     uint8
    flag       uint8
    _          [1]byte
    nfuncdata  uint8  // number of funcdata entries (must be last, on a uint32 boundary)
}
```

Immediately after the `_func` struct come **two variable-length arrays**:

```text
[ _func fixed part ][ pcdata[npcdata] uint32 ][ funcdata[nfuncdata] uint32 ]
```

- `pcdata[i]` is "the i-th PC-encoded table's offset in pctab" (`pcdatavalue`
  uses it to find the table, then looks up the value by PC);
- `funcdata[i]` is "the i-th funcdata's offset in the gofunc block" (`funcdata()`
  returns `gofunc + funcdata[i]`).

This library uses only two pcdata tables (`StackMapIndex`, `ArgLiveIndex`, both
0 meaning "no table") and two funcdata entries (`ArgsPointerMaps`,
`LocalsPointerMaps`).

## pcvalue: varint-encoded PC tables

Tables like `pcsp` (stack pointer delta) are decoded with `pcvalue`. A table is
a sequence of `(uvdelta, pcdelta)` pairs, both varints:

- `uvdelta` is the **zigzag-encoded value increment**: `val += -(uvdelta&1) ^
  (uvdelta>>1)`, starting at `val = -1`;
- `pcdelta` is the **PC increment**, in units of `PCQuantum`: `pc += pcdelta *
  PCQuantum`;
- the table ends at `uvdelta == 0` (the `step` function treats 0 as the end
  marker when the `first` flag is false).

This explains the two pitfalls of `encodePCSP` (the pcsp-table generator) in the
JIT:

1. **pcdelta must be non-zero**: `pcvalue` uses `pc == entry()` as its "first"
   flag; if pcdelta were 0, `first` would stay true forever and the end marker 0
   would be misread as a real pair, driving `step` past the slice. So the table
   must carry a non-zero pcdelta covering the whole function.
2. **the `pcsp` field must not be 0**: `pcvalue` treats `off == 0` as "no table"
   and returns -1, and `_func.pcsp == 0` makes `findfunc` treat the function as
   external. So the JIT's pcsp table lives at `pctab[1]`, with `pctab[0]` left
   as a sentinel.

There is a subtler pitfall: **spdelta is not constant**. The trampoline's
prologue (`PUSH` before `SUB`) and epilogue (`ADD` before `POP`) leave SP at
intermediate values. If the pcsp table encoded one constant `jitSPDelta` for the
whole function, an async preemption landing just before the epilogue's `POP`
would make `funcspdelta` return the wrong value and traceback would fail with
"did not unwind completely". So `encodePCSP` encodes the real ranges:
`0 → 8 → jitSPDelta → 8 → 0` (amd64), writing every SP change point of the
prologue/epilogue into the table.

## stackmap: the pointer bitmap

What the GC ultimately wants is a `stackmap`:

```go
type stackmap struct {
    n        int32  // number of bitmaps
    nbit     int32  // bits per bitmap (one bit per word)
    bytedata [1]byte // bitmap data, variable length
}
```

`getStackMap`'s logic: first get the bitmap index from `pcdatavalue(StackMapIndex)`;
if that is unavailable (-1) it falls back to `funcdata(ArgsPointerMaps)` /
`funcdata(LocalsPointerMaps)` to fetch the stackmap directly. `stackmapdata`
then takes the n-th bitmap to obtain `bitvector{nbit, bytedata}`.

**One crucial detail**: after `funcdata` returns `gofunc + off`, the runtime
**dereferences that address directly as a `*stackmap`**. So the stackmap must be
**inlined** in the gofunc block (`n`/`nbit`/`bytedata` contiguous), not stored
as a pointer to a stackmap — a pointer would have the runtime misread the
pointer value as `n`/`nbit`.

## The full loop

Stringing the chain together is what the GC does to a JIT frame during a stack
scan:

```text
GC scans stack → gentraceback walks frames → findfunc for each frame's PC
  → findmoduledatap hits the JIT module → findfuncbucket → ftab → _func
  → getStackMap obtains the stackmap via pcdata/funcdata
  → decide word by word: bit=1 is a pointer, trace/adjust; bit=0 is an integer, skip
```

JIT forges a moduledata, which is essentially **filling in, by hand, every table
this chain needs**, so the runtime treats an mmap'd piece of machine code as a
normal, self-describing Go function. No black magic, just getting the public
format right.
