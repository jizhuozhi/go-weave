# go-weave 技术白皮书

这个项目的初衷，是证明 Go 可以像 Java 一样，通过动态代理实现 AOP（面向切面编程）。

这份文档体系的目标，是让一个只写过常规 Go 代码的读者，读完能**完整理解 go-weave 为何能工作**——不是靠运行时内部未公开的黑魔法，而是对 Go 已经公开承诺（或事实稳定）的机制的**系统运用**。

go-weave 做了一件看起来不可能的事：在运行时伪造一个 `itab`，让一个从未声明"实现了 `T`"的类型 `*Proxy` 成为一个合法的接口值。它没有 patch 任何字节码、没有 hook 任何 runtime 内部函数。它只是：

1. 理解接口值就是 `(itab, data)` 两个词，自己造一个 `itab`；
2. 理解接口调用 `CALL (R6)` 需要裸代码指针，就用运行时 JIT 生成一段机器码；
3. 理解 GC 扫栈靠 pclntable 里的指针 map，就伪造一个自洽的 `moduledata` 让 GC 能识别这段机器码；
4. 理解参数在寄存器/栈里的 ABI 布局，就复刻寄存器分配算法把它们转译成 Go 值。

每一步都有对应的 runtime 机制支撑。文档按这个依赖关系组织，从底层概念一层层搭上去。

本库用到的每一项技术，要么是 Go 发行版公开的机制（接口值的双字布局、寄存器 ABI、pclntable 的公开格式），要么是少数几个不得已 pin 死在 runtime 内部布局上的 `//go:linkname` 符号——后者正是 Go 源码"耻辱柱"（hall of shame）注释里点名的用法，与 sonic 同列，其长期稳定性可参考 rsc 在 GitHub issue（如 go.dev/issue/67401、71672）里的讨论。

## 阅读顺序

文档按编号阅读，每一篇都只依赖它前面的概念：

1. **[01-interface.md](01-interface.md) — 接口调用与 itab**：接口值的双字布局（胖指针）、itab 结构、为什么方法表必须是裸代码指针。这是整个问题的起点。

2. **[02-register-abi.md](02-register-abi.md) — 寄存器 ABI**：参数在寄存器/栈之间如何分配，home slot、outgoing area、指针位图。回答"参数在哪、哪个是指针"。

3. **[03-runtime.md](03-runtime.md) — runtime 栈与 GC**：GMP、栈分裂、栈移动（copystack 如何调整指针）、三色标记、栈扫描如何靠指针 map 判定。这是理解"为什么指针必须对 GC 可见"的底座。

4. **[04-pclntab.md](04-pclntab.md) — pclntable 白盒**：可执行文件如何自描述——pcHeader、`_func`、pcvalue 的 varint 编码、funcdata、stackmap、findfunc 的桶查找。这是理解"为什么 JIT 要伪造 moduledata"的前提。

5. **[05-jit.md](05-jit.md) — 桩与 JIT**：机器码的结构、moduledata 如何逐字段对应 runtime 的读取、两种桩（通用/精确）、版本分段、MAP_JIT 平台 shim。

6. **[06-dispatch.md](06-dispatch.md) — Dispatch 与调用路径**：寄存器如何进入 Dispatch、拦截器链如何执行、fast path 与 reflect fallback、materialize/scatter。

## 概念地图

```
       01 接口值 (itab, data)
              │  Fun[k] 必须是裸代码指针
              ▼
       05 JIT 生成机器码 + 伪造 moduledata
          │                          │
          │ 参数怎么传                │ GC 怎么认识这段代码
          ▼                          ▼
   02 寄存器 ABI              04 pclntable (findfunc → 指针 map)
          │                          │
          └──────────┬───────────────┘
                     ▼
              03 runtime 栈与 GC（扫栈、栈移动、指针判定）
                     │
                     ▼
              06 Dispatch（转译 + 拦截器链 + 回写）
```

核心的闭环是：**接口调用把一个裸指针 `CALL` 进来 → JIT 造一段能被 `findfunc` 识别的代码 → GC 靠 pclntable 的指针 map 知道栈上哪些字是指针 → 参数按 ABI 转译成 Go 值走拦截器链**。每一环都有对应的 runtime 机制，没有任何一环是"碰运气"。

## 术语索引

| 术语 | 含义 | 详见 |
|---|---|---|
| 接口值 / `iface` / `eface` | 两个机器字：`(itab, data)` 或 `(_type, data)` | 01 |
| 胖指针（fat pointer） | 携带类型元数据的数据指针 | 01 |
| itab / `Fun[k]` | 接口分发表，方法入口代码指针数组 | 01 |
| 裸代码指针 | 不带闭包上下文的函数入口地址 | 01 |
| 寄存器 ABI / home slot | Go 的参数传递约定，寄存器参数在调用方帧里的"家" | 02 |
| outgoing area / 栈参数区 | 调用方为被调方栈参数预留的区域 | 02 |
| 指针位图 / `ptrMask` | 标记哪些寄存器/栈字是指针 | 02 |
| GMP | goroutine / OS 线程 / 逻辑处理器 | 03 |
| 栈分裂 / `morestack` | 函数序言的扩栈检查 | 03 |
| 栈移动 / `copystack` | 复制栈并调整所有指针 | 03 |
| 三色标记 / 写屏障 | GC 的标记算法与增量安全机制 | 03 |
| 指针 map / `stackmap` / bitvector | 描述栈上哪些字是指针的位图 | 03, 04 |
| pclntable / `pcHeader` / `_func` | 可执行文件里描述函数元数据的表 | 04 |
| pcdata / funcdata / pcvalue | 函数级元数据与 pc 编码表 | 04 |
| `findfunc` / `findfuncbucket` / `ftab` | 由 PC 定位函数元数据的查找链 | 04 |
| moduledata | 一个可加载模块的元数据集合 | 04, 05 |
| MAP_JIT / write-protect | Apple Silicon 可执行页的写入协议 | 05 |
| `uintptr` vs `unsafe.Pointer` | 前者 GC 不追踪、后者追踪 | 03 |
