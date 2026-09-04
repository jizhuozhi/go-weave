# 寄存器 ABI

桩是"最大寄存器签名"的裸代码指针。要把接收到的参数转译成 Go 值、再把结果写回，就必须复刻 Go 的寄存器分配算法。本文说明参数如何在寄存器与栈之间分配。这是后续 `materialize`/`scatter` 的基础。

## 为何需要这份算法

接口调用发生时，参数已按 Go 的 ABI 就位：整数与指针在整数寄存器（arm64 为 R0–R15，amd64 为 AX/BX/CX/DI/SI/R8–R11），浮点在浮点寄存器，放不下的落入调用者的栈参数区。桩原样接收后，`Dispatch` 需要知道"第 i 个参数在何处"，才能：

- 把参数 `materialize` 为 `reflect.Value` 交给拦截器；
- 把拦截器改写后的结果 `scatter` 回正确的寄存器/栈位置。

`abi.go` 是 `reflect/abi.go` 中寄存器分配部分的私有移植（`reflect` 的实现未导出），基于 `reflect.Type` 实现。

## 算法：一个"步骤"序列

每个参数被展开为若干 `step`，每个 step 是一条传送指令：

```go
type step struct {
    kind   stepKind // stepStack / stepIntReg / stepPointer / stepFloatReg
    offset uintptr  // 在 Go 值内部的偏移
    size   uintptr  // 该步的字节数
    stkOff uintptr  // 栈参数区内的偏移（stepStack）
    ireg   int      // 整数寄存器下标
    freg   int      // 浮点寄存器下标
}
```

规则（`regAssign`）：

- 指针/chan/map/func → 1 个整数寄存器，标记 `stepPointer`；
- 整数/布尔 → 1 个整数寄存器；
- 浮点 → 1 个浮点寄存器（complex 占 2 个）；
- `string` → 2 个整数寄存器（data 是指针、len 不是），指针位图 `0b01`；
- `interface` → 2 个整数寄存器（itab 与 data 都是指针），`0b10`；
- `slice` → 3 个整数寄存器（ptr/len/cap），`0b001`；
- `struct` → 逐字段递归，任一字段无法放入则整体失败；
- **一旦某参数无法放入剩余寄存器，它整体落入栈，其后所有参数也都留在栈上**（Go ABI 的"溢出即整体入栈"规则）。

## 栈参数区与 home slot

无法放入寄存器的参数进入调用者的 **outgoing area**（栈参数区）。`abiLayout` 记录几个关键量：

- `stackCallArgsSize`：栈参数的字节数（`Dispatch` 从 `&s0` 拷贝这些字节到池化缓冲）；
- `retOffset`：栈结果的起始位置（与参数不共享空间，按 `ptrSize` 对齐）；
- `stackBytes`：整个参数区（参数与结果）。

还有一个概念贯穿始终：**home slot**。寄存器参数在调用方帧里也有一块"家"（按 `_func.args` 预留），供栈增长时 morestack 保存寄存器。接口调用点按**方法自身签名**预留 home slot，而桩的签名是"最大寄存器文件"，二者的 home slot 大小不同——这就是为什么桩不能有栈检查（`//go:nosplit`）的原因，详见 [03-runtime.md](03-runtime.md) 的 morestack 部分。

## 指针位图

两处指针信息是 GC 安全的关键：

- **`ptrMask`**（整数寄存器）：bit i 表示第 i 个整数参数寄存器是否持指针。`Dispatch` 仅把持指针的寄存器拷入 GC 可见的 `ptrs` 镜像，其余留在 `uintptr` 中不被扫描——否则一个恰好与堆地址相似的普通整数会被 GC 误判为指针，触发 "found bad pointer in Go heap"。
- **`stackPtrOffs` / `retStackPtrOffs`**（栈参数区）：栈参数/结果中每个指针的字节偏移。它们决定方法是否需要"精确桩"，见 [05-jit.md](05-jit.md)。

## 一个完整布局

`newABILayout` 对方法类型计算一遍：先 `addRcvr`（receiver 固定占第一个整数寄存器，指针位），再逐个 `addArg` 参数，最后计算结果。产物 `abiLayout` 是后续 `materialize`/`scatter`/`shape` 的全部输入。
