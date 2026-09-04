# 桩与 JIT

`itab.Fun[k]` 需要裸代码指针，本库在运行时生成它：一段机器码（桩）+ 一个自洽的 `moduledata`（让 runtime 认识这段机器码）。本文说明两者如何逐字段构造。pclntable 的格式见 [04-pclntab.md](04-pclntab.md)。

## 两种桩

| 桩 | 生成时机 | 用途 |
|---|---|---|
| **通用桩**（128 个 slot） | 启动 `init` 时 prefetch | 无指针栈参数的方法，一个 slot 服务于所有接口的 method k |
| **精确桩** | 代理构造时按 shape 生成并缓存 | 有指针经过栈参数区的方法 |

两种桩的机器码几乎一致，差别仅在 `moduledata` 的指针 map：通用桩声明参数区无指针（`argPtrs = 0`），精确桩逐字声明（`argPtrs`/`retPtrs` 标记每个持指针的字）。

## 机器码逐条拆解

`jitcode_arm64.go` 的 `jitStubCode` 通过 `asm` 把 AArch64 GNU 语法字符串解析成机器码，所以桩体读起来就是汇编（amd64 用 Intel 语法，机制相同）：

```go
put(asm("SUB R20, SP, #%d", jitFrameSize)) // 先算新 SP 到 R20（288 = jitFrameSize）
put(asm("STP R29, R30, [R20, #-8]"))       // 把旧 FP、LR 存到新帧
put(asm("ADD SP, R20, #0"))                // MOV SP, R20（用 ADD #0：ORR 的 r31 是 ZR，不是 SP）
put(asm("SUB R29, SP, #8"))                // 设本帧的 frame pointer
```

`asm` 是个轻量汇编器：`tokenize` 按分隔符切分，`parseReg`/`parseImm` 解析操作数，`parseIns` 按指令名分派到编码 helper（`subImm`/`strOff`/`movz`…）。每个 helper 只做位拼接，注释写清字段布局。它只认 `jitStubCode` 用到的指令子集，拼错的指令会在 `init` 预生成时 panic 暴露。

前四条是标准的函数序言，和编译器生成的一样。关键是它**没有 `morestack` 检查**（`//go:nosplit` 语义），帧大小固定，所以 `pcsp` 能编码成一个常量。

```go
put(asm("STR R15, [SP, #8]"))               // 第 16 个整数寄存器溢出到栈槽
put(asm("ADD R16, SP, #%d", jitFrameSize+8)) // 算 &s0（调用者栈参数区起点）
put(asm("STR R16, [SP, #16]"))               // &s0 放进下一个溢出槽
```

`Dispatch` 的签名是 `(idx int, a0..a15 uintptr, f0..f15 float64, stack unsafe.Pointer)`——`idx` + 16 个整数 + 1 个栈指针，超过 16 个整数寄存器的容量，所以第 17 个（`a15`）和第 18 个（`stack`）溢出到栈。这两条 `STR` 就是填这两个溢出槽。

```go
for r := 14; r >= 0; r-- {
    put(asm("MOV R%d, R%d", r+1, r)) // 整数寄存器整体右移一格
}
put(asm("MOVZ R0, #%d", sh.index))   // 腾出 R0 放 slot 下标
```

为什么右移：接口调用时 receiver 在 R0，`Dispatch` 的 `a0` 参数也约定落在 R1。把 R0→R1、R1→R2、…、R14→R15，腾出 R0 放 `idx`，于是 `Dispatch` 里 `ints[0] == a0 == receiver`。

```go
put(asm("MOVZ R16, #%d", uint16(dispatch)))        // 分段加载 64 位绝对地址
put(asm("MOVK R16, #%d, LSL #16", uint16(dispatch>>16)))
put(asm("MOVK R16, #%d, LSL #32", uint16(dispatch>>32)))
put(asm("MOVK R16, #%d, LSL #48", uint16(dispatch>>48)))
put(asm("BLR R16"))                                // 跳到 Dispatch
```

arm64 的 `BL` 只够 ±128MB，mmap 页和 text 段之间的距离可能超出，所以用 `MOVZ`/`MOVK` 把 `Dispatch` 的 64 位地址直接装进 R16 再 `BLR`。

```go
put(asm("LDP R29, R30, [SP, #-8]")) // 恢复 FP、LR
put(asm("ADD SP, SP, #%d", jitFrameSize))
put(asm("RET"))
```

返回前恢复 frame pointer、link register 和 SP。

精确桩额外在 `BLR` 之前**清空结果字里被标记为指针的那些**（`STR ZR, [R16, #w*8]`）：这些字在调用前仍保存着调用方旧帧内容，而新的参数 map 已声明它们是指针，若在第一个 safe point 前发生 GC，就会读到失效数据。桩是纯机器码，`BLR` 之前没有 safe point，先清空即可。

## moduledata 逐字段填法

`buildJITModule` 对照 [04-pclntab.md](04-pclntab.md) 的格式，把 findfunc/GC 需要的每一张表填上：

| 字段 | 填什么 | 对应 runtime 的读取 |
|---|---|---|
| `pcHeader` | `magic=0xfffffff1`、`minLC=PCQuantum`、`ptrSize=8`、`nfunc=1` | `moduledataverify1` 校验 magic/minLC/ptrSize |
| `funcnametab` | `"\0weave.jitstub\0"` | `Func.Name()` 读名字 |
| `pctab` | `[0]` 哨兵 + `jitPCSPTable` | `pcvalue` 解码 pcsp |
| `pclntable` | 一个 `_func`（含 pcdata/funcdata 两个尾数组） | `findfunc` 定位、`getStackMap` 读位图 |
| `ftab` | 两行：`{0,0}` + 末尾哨兵 | `findfunc` 的桶查找收敛 |
| `findfunctab` | 一个 bucket，`idx=0`、subbuckets 全 0 | `findfunc` 第 3、4 步 |
| `minpc`/`maxpc` | `text` / `etext` | `findmoduledatap` 的范围匹配 |
| `gofunc` | 内嵌的 args map + locals map | `funcdata` 返回 `gofunc+off` 后直接解引用 |

`registerModule` 通过 `//go:linkname` 把模块链接到 `runtime.lastmoduledatap` 链表尾，`findfunc` 即可遍历到它。一旦注册，页及其引用的所有分配都必须存活至进程结束，因此全部保存进 `jitRoots` 永久保活。

## 版本分段镜像

`moduledata` 与 `_func` 的字段顺序随 Go 版本变化，镜像按段拆分：

| 结构 | 版本段 |
|---|---|
| `moduledata` | 1.18/1.19 基准；1.20 +`covctrs`；1.21 +`inittasks`；1.23 `bad` 移至 `hasmain` 后；1.26 +`epclntab`；1.27 删 `typelinks`/`itablinks`、+`typedesclen`/`itaboffset`/`itabsize` |
| `_func`（`rfunc`） | 1.18/1.19 无 `startLine`；1.20+ 有 `startLine` |

每段自带 `TestJITFindfunc` 校验的期望 offset（`text`/`pctab`/`gofunc`/`next`），CI 在 1.18–1.27 每个版本上跑，任何一段写错都会当场暴露。

## 平台 shim：MAP_JIT

Apple Silicon 上 `MAP_JIT` 页默认 RX，写代码前需 `pthread_jit_write_protect_np` 切至 RW、写完后切回 RX，并 `sys_icache_invalidate` 刷新指令缓存。这些 C 调用隔离在 `internal/rt`（`jitmem_darwin_arm64.go` 通过 `jit_darwin.go` 调用），其他平台走普通 RWX `mmap`（`jitmem.go`）。
