# runtime 栈与 GC

本库大量依赖 Go runtime 的底层行为：寄存器 spill 要对 GC 可见、栈移动不能留下悬空指针、`uintptr` 与 `unsafe.Pointer` 的取舍。本文把这些机制拆开，说明"怎么做的"，而不仅是"做了什么"。

## GMP 与 goroutine 栈

- **G**（goroutine）：用户态轻量线程，拥有自己的调用栈；
- **M**（machine）：OS 线程，实际执行代码的载体；
- **P**（processor）：逻辑处理器，持有本地 runq 与调度上下文，决定哪些 G 在哪个 M 上运行。

栈是 G 的一部分：每个 goroutine 一段连续内存，初始约 2KB，按需增长，上限 1GB（`runtime.maxstacksize`）。栈会**分裂**、会**移动**，这是下面两个机制。

## 栈分裂：morestack

每个函数序言里有一条栈检查——把 SP 和 `g.stackguard0` 比较，SP 越过 guard 就跳到 `runtime.morestack`：

```
函数序言（编译器生成）:
    CMP SP, stackguard
    BLO morestack        // 栈不够了
    ... 正常执行
```

`morestack` 保存现场后进入 `runtime.newstack`，后者分配更大的栈并触发 `copystack`。本库的 JIT 桩是纯机器码，**没有这条检查**（等价于编译期 `//go:nosplit`），帧大小固定，因此 `funcspdelta` 报告常量，traceback 才能正确 unwind——这在本库 [04-pclntab.md](04-pclntab.md) 的 pcvalue 部分有对应。

## 栈移动：copystack 如何调整指针

扩栈时 `copystack` 分配一块新栈，把旧栈内容复制过去。难点在于：**旧栈上的指针现在都指向错误的位置**，必须全部改成新地址。做法是：

```go
func copystack(gp *g, newsize uintptr) {
    old := gp.stack
    new := stackalloc(newsize)          // 1. 分配新栈
    memmove(new, old, used)             // 2. 复制内容
    adjinfo := adjustinfo{old, new}
    gentraceback(..., adjustframe, ...) // 3. 遍历每一帧，调整指针
    ...
}
```

`adjustframe`（`runtime/stack.go`）对每一帧：

```go
locals, args, objs := frame.getStackMap(true)  // 拿这一帧的指针位图
adjustpointers(frame.varp-size, &locals, ...)  // 调整局部变量里的指针
adjustpointers(frame.argp, &args, ...)         // 调整参数区里的指针
```

`adjustpointers` 按位图逐字处理：bit 为 1 的字是指针，减去 `old` 加上 `new` 得到新地址；bit 为 0 的字跳过。

**关键陷阱由此而来**：只有**被位图标记为指针**的值会被调整。栈上一个 `uintptr` 保存的是栈地址的位模式，位图不认为它是指针，`adjustpointers` 不会碰它，栈移动后它就变成悬空地址。

这正是本库要把调用现场放在堆上的原因——`materialize.go` 的 `callState` 注释：

> *Keeping all of it off the goroutine stack also makes it immune to stack moves — a stack growth inside an interceptor would leave stack-allocated state dangling.*

栈参数区的原始指针（`&s0`）从不进入堆对象：`Dispatch` 把它拷贝进池化的 `stackBuf`，拦截器内即使发生栈增长，这份拷贝也稳定地保留在堆上。

## GC 与栈扫描

GC 是三色标记，**栈是根**。标记阶段从根出发，扫栈时 `gentraceback` 遍历每一帧，对每帧 `getStackMap` 拿 locals/args 两个 bitvector，逐字判定：bit 为 1 的字是指针，要追踪（作为新的标记源）；bit 为 0 的字当整数，跳过。

这些 bitvector 来自 pclntable 的 `stackmap`（见 [04-pclntab.md](04-pclntab.md)）。**一个 mmap 出的裸页不在任何 moduledata 里**，`findfunc` 找不到它的 `_func`，`getStackMap` 返回空，GC 直接 `missing stackmap` 崩溃。这正是 JIT 必须伪造 moduledata 的原因。

写屏障是另一块拼图：并发标记期间，指针的写入要经过写屏障记录，保证标记不丢。它对本库的影响是间接的——只要指针被正确放进 `unsafe.Pointer` 字段（见下节），写屏障就会替我们处理好。

`GODEBUG=clobberfree=1` 的测试就是在验证这套链路：被回收的内存被打上哨兵值，任何"漏扫指针"（栈上一个应该被标记却没被标记的字）都会立刻读到哨兵值而崩溃。

## unsafe.Pointer 与 uintptr

| | 语义 | GC 标记 | 栈移动 |
|---|---|---|---|
| `unsafe.Pointer` | 指针，可跨 GC 持有 | 追踪 | 调整 |
| `uintptr` | 整数，只是地址的位模式 | **不追踪** | **不调整** |

推论：

- 把指针转为 `uintptr` 后，只要原指针不再被引用，GC 就可能回收对象，`uintptr` 即变为悬空地址。因此 `uintptr` 是**瞬态**的：`p := unsafe.Pointer(uintptr(x) + off)` 必须在同一表达式内转回指针，跨语句或跨 GC 持有 `uintptr` 是错误的。
- 需要"临时把指针降级为不追踪的整数"时（例如避免 GC 误判一个与堆地址相似的整数），使用 `uintptr` 则是有意为之。

本库中有两处应用：

1. **`regBuf` 双镜像**（`materialize.go`）：`ints [16]uintptr` 保存原始寄存器位模式（GC 不扫），`ptrs [16]unsafe.Pointer` 只填 `ptrMask` 标记为指针的寄存器（GC 扫这个）。缺少前者，GC 会把普通整数误判为指针而触发 "found bad pointer in Go heap"；缺少后者，则会漏掉真正的指针参数。

2. **`Dispatch` 返回前**（`dispatch_*.go`）：结果是从 `uintptr` 转回的 `unsafe.Pointer`，spill 在本帧内，注释明确 *"No defer on the Put, and no call after this point"*——保证从转换到返回之间没有任何 GC 机会观察到这些尚未完成的转换结果。
