# pclntable 白盒

要理解"JIT 为什么必须伪造一个 moduledata"，先要理解 Go 可执行文件如何**自描述**——给定一个程序计数器（PC），runtime 如何知道它属于哪个函数、函数有哪些元数据、栈上哪些字是指针。这套机制叫 pclntable（program counter line table）。

## 一句话概括

pclntable 是一组表，回答两个问题：

1. **这个 PC 属于哪个函数**（`findfunc`）；
2. **这个函数在某个 PC 处的栈布局是什么**（pcdata/funcdata → 指针 map）。

GC 扫栈、traceback 展开、`runtime.Caller` 全都依赖它。一个可执行段如果不在这些表里，runtime 就"看不见"它——这正是 mmap 裸页会 `missing stackmap` 崩溃的原因。

## 查找链：从 PC 到函数

`findfunc`（`runtime/symtab.go`）的完整链路：

```go
func findfunc(pc uintptr) funcInfo {
    datap := findmoduledatap(pc)   // 1. 哪个模块
    if datap == nil { return funcInfo{} }

    pcOff, ok := datap.textOff(pc) // 2. PC 相对模块 text 的偏移
    if !ok { return funcInfo{} }

    b := uintptr(pcOff) / abi.FuncTabBucketSize          // 3. 哪个 bucket
    i := uintptr(pcOff) % abi.FuncTabBucketSize / (FuncTabBucketSize / nsub) // 哪个 subbucket

    ffb := (*findfuncbucket)(add(datap.findfunctab, b*unsafe.Sizeof(findfuncbucket{})))
    idx := ffb.idx + uint32(ffb.subbuckets[i])           // 4. 候选函数下标

    for datap.ftab[idx+1].entryoff <= pcOff { idx++ }    // 5. 线性收敛到真正函数
    funcoff := datap.ftab[idx].funcoff
    return funcInfo{(*_func)(unsafe.Pointer(&datap.pclntable[funcoff])), datap}
}
```

每一步：

1. **`findmoduledatap`**：遍历 `moduledata` 链表（`firstmoduledata → next`），找 `minpc <= pc < maxpc` 的那个模块。JIT 的 `registerModule` 就是把伪造的模块挂到这个链表尾。
2. **`textOff`**：`pc - md.text`，得到 PC 在模块代码段里的偏移。
3. **`findfuncbucket`**：把 text 按 `FuncTabBucketSize`（4096 字节）切成 bucket，每个 bucket 里 16 个 subbucket（各覆盖 256 字节）。bucket 头存一个 `idx`（起始函数下标）+ 16 个 `subbuckets` 字节（每个是相对 idx 的增量）。这样 O(1) 就收敛到一个小范围，而不是在整张 `ftab` 上二分。
4. **`ftab`**：`[]functab{entryoff, funcoff}`，按 entryoff 排序。`entryoff` 是函数入口在 text 里的偏移，`funcoff` 是该函数 `_func` 在 `pclntable` 里的偏移。
5. 线性前进，直到 `ftab[idx+1].entryoff > pcOff`，`idx` 就是目标函数。

JIT 模块只有一个函数，所以 `findfunctab` 只需一个 bucket（`idx=0`、`subbuckets` 全 0），`ftab` 两行（函数 + 末尾哨兵）。

## pcHeader：模块级的表头

```go
type pcHeader struct {
    magic          uint32 // 0xfffffff1，Go 1.20+ 的 PCLnTabMagic
    pad1, pad2     uint8
    minLC          uint8  // 最小指令长度（PCQuantum）
    ptrSize        uint8  // 指针大小
    nfunc          int    // 函数数量
    nfiles         uint   // 文件数
    textStart      uintptr
    funcnameOffset uintptr // 到 funcnametab 的偏移
    cuOffset       uintptr
    filetabOffset  uintptr
    pctabOffset    uintptr // 到 pctab 的偏移
    pclnOffset     uintptr // 到 pclntable 的偏移
}
```

`magic` 是版本号，`moduledataverify1` 在启动时校验它——写错 magic 会直接 `invalid function symbol table` 崩溃。`minLC`（= `PCQuantum`，arm64 为 4、amd64 为 1）是 pcvalue 表里 PC 增量的单位，下面会用到。

## _func：单个函数的元数据

`pclntable` 里的每一行是 `_func`（本库镜像为 `rfunc`）：

```go
type _func struct {
    entryoff   uint32 // 入口相对 text 的偏移
    nameoff    int32  // 名字在 funcnametab 里的偏移
    args       int32  // 参数区大小（字节）
    deferreturn uint32
    pcsp       uint32 // pcsp 表在 pctab 里的偏移
    pcfile     uint32
    pcln       uint32
    npcdata    uint32 // pcdata 表的项数
    cuOffset   uint32
    startLine  int32  // Go 1.20 加入
    funcID     uint8
    flag       uint8
    _          [1]byte
    nfuncdata  uint8  // funcdata 表的项数（必须为末字段，落在 uint32 对齐边界）
}
```

`_func` 结构体之后**紧跟着两个变长数组**：

```
[ _func 定长部分 ][ pcdata[npcdata] uint32 ][ funcdata[nfuncdata] uint32 ]
```

- `pcdata[i]` 是"第 i 张 pc 编码表在 pctab 里的偏移"（`pcdatavalue` 靠它找到表，再按 PC 查值）；
- `funcdata[i]` 是"第 i 份 funcdata 在 gofunc 数据块里的偏移"（`funcdata()` 返回 `gofunc + funcdata[i]`）。

本库只用到两张 pcdata（`StackMapIndex`、`ArgLiveIndex`，值全 0 表示"无表"）和两份 funcdata（`ArgsPointerMaps`、`LocalsPointerMaps`）。

## pcvalue：varint 编码的 PC 表

`pcsp`（stack pointer delta）这类表用 `pcvalue` 解码。表是一串 `(uvdelta, pcdelta)` 对，都是 varint：

- `uvdelta` 是 **zigzag 编码的值增量**：`val += -(uvdelta&1) ^ (uvdelta>>1)`，起始 `val = -1`；
- `pcdelta` 是 **PC 增量**，单位是 `PCQuantum`：`pc += pcdelta * PCQuantum`；
- 表以 `uvdelta == 0` 结束（`step` 函数在 `first` 标志为假时把 0 当结束标记）。

这解释了 JIT 里 `jitPCSPTable` 的两个坑：

1. **pcdelta 必须非零**：`pcvalue` 用 `pc == entry()` 当 "first" 标志，若 pcdelta 为 0，`first` 永远为真，结束标记 0 会被误读成一对数据，`step` 越界。所以表里要写一个覆盖整个函数的非零 pcdelta。
2. **`pcsp` 字段不能是 0**：`pcvalue` 把 `off == 0` 当作"无表"返回 -1，而 `_func.pcsp == 0` 会让 `findfunc` 把它当外部函数处理。所以 JIT 的 pcsp 表存在 `pctab[1]`，`pctab[0]` 留作哨兵。

## stackmap：指针位图

GC 最终要的是 `stackmap`：

```go
type stackmap struct {
    n        int32  // 位图数量
    nbit     int32  // 每个位图的位数（一字一位）
    bytedata [1]byte // 位图数据，变长
}
```

`getStackMap` 的逻辑：先用 `pcdatavalue(StackMapIndex)` 拿位图下标，拿不到（-1）就回退到 `funcdata(ArgsPointerMaps)` / `funcdata(LocalsPointerMaps)` 直接取 stackmap。`stackmapdata` 再按 `n` 取第 n 个位图，得到 `bitvector{nbit, bytedata}`。

**一个关键细节**：`funcdata` 返回 `gofunc + off` 后，runtime **直接把那个地址当 `*stackmap` 解引用**。所以 stackmap 必须**内联**在 gofunc 数据块里（`n`/`nbit`/`bytedata` 连续存放），而不是存一个指向 stackmap 的指针——存指针会让 runtime 把指针值误读成 `n`/`nbit`。

## 完整闭环

把这条链串起来，就是 GC 扫栈时对一个 JIT 帧做的事：

```
GC 扫栈 → gentraceback 遍历帧 → 对每帧 PC 调 findfunc
  → findmoduledatap 命中 JIT 模块 → findfuncbucket → ftab → _func
  → getStackMap 用 pcdata/funcdata 取 stackmap
  → 按位图逐字判定：bit=1 是指针，追踪/调整；bit=0 是整数，跳过
```

JIT 伪造 moduledata，本质就是**手工填满这条链所需的每一张表**，让 runtime 把一段 mmap 出来的机器码当作一个正常的、自描述的 Go 函数。没有黑魔法，只是把公开格式填对。
