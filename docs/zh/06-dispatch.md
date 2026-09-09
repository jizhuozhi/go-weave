# Dispatch 与调用路径

桩把寄存器 shuffle 进 `Dispatch`，`Dispatch` 是运行时逻辑的入口：把裸寄存器转译成拦截器可操作的 Go 值，执行拦截器链，再把结果写回寄存器。本文说明这条路径的两端——入口如何接收，链尾如何返回。

## 入口：寄存器如何进入

`Dispatch` 的签名是架构的"最大寄存器文件"（`dispatch_arm64.go` / `dispatch_amd64.go`）：

```go
func Dispatch(idx int, a0, ..., a15 uintptr, f0, ..., f15 float64, stack unsafe.Pointer) (...)
```

对应桩执行的 shuffle（见 [05-jit.md](05-jit.md) 的机器码拆解）：

- `idx` 由桩硬编码进 R0（`MOVZ $idx, R0`）；
- `a0` = 原 R0 = **receiver**（接口调用总是把 receiver 放在第一个整数寄存器）；
- 其余 `a1..a15` 是原 R1..R15 顺延；
- `stack` = `&s0`，调用者栈参数区指针（桩写入溢出位）。

`idx` 是方法在接口中的下标，也是 `itab.Fun` 的下标。`Dispatch` 用它从 receiver 的代理解析出具体 `*Method`——因此**一个 slot 服务于所有接口的 method k**，slot 数仅受"每个接口的方法数"限制。

## 寄存器落位与 GC 安全

`Dispatch` 首先把寄存器写入池化的 `callState`（`materialize.go` 的 `regBuf`）：

```go
type regBuf struct {
    ints   [intArgRegs]uintptr       // 原始位模式，GC 不扫
    ptrs   [intArgRegs]unsafe.Pointer // 只填持指针的寄存器，GC 扫这个
    floats [floatArgRegs]float64
}
```

`ints` 是 `uintptr`，GC 不扫描；`ptrs` 只放 `layout.ptrMask` 标记为指针的那些寄存器（`Dispatch` 中的 `prePtrs` 循环）。这种拆分使 GC 既不漏掉指针参数，也不会把一个恰好与堆地址相似的普通整数当作指针而 abort。`uintptr` 与 `unsafe.Pointer` 的区别见 [03-runtime.md](03-runtime.md)。

栈参数同理：`Dispatch` 把 `&s0` 起的 `stackCallArgsSize` 字节拷入池化的 `stackBuf`，拦截器内的栈移动不会留下悬空指针。

## 链尾的两条路径

`Invocation.Proceed`（`invocation.go`）执行完拦截器链后，按是否 materialize 过参数分两条路径返回 target：

**fast path（`redial`）**——`c.args == nil && stackBytes == 0`：

```go
c.regs.ints[0] = uintptr(m.targetData)  // 只换 receiver
redial(m.targetFun, c.regs)             // 寄存器原样重放给 target
c.direct = true
return nil
```

`redial_*.s` 把 `regs` 中的寄存器重新加载、`CALL` target 的裸代码指针、再把结果寄存器存回。**全程不经过 reflect、不 box 参数、零分配**——这是 `ProxyAdd` 能达到 47ns / 0 allocs 的原因。

**reflect fallback**——拦截器访问过参数（`Args()` 惰性触发 `materializeArgs`），或存在栈参数：

```go
return m.targetFn.Call(c.Args())  // 或 CallSlice（variadic）
```

一旦参数被 `materialize` 为 `[]reflect.Value`，就走 `reflect.Value.Call`，慢一个数量级（约 265ns / 6 allocs）。

## materialize 与 scatter

`materialize.go` 是"寄存器 ↔ reflect.Value"的编解码层：

- **`materializeArgs`**：按 `abiLayout` 的 step 序列，把寄存器/栈中的原始位模式还原为 `[]reflect.Value`。单寄存器/单栈槽的参数直接用 `reflect.NewAt(...).Elem()` 原位描述（零拷贝）；跨多个寄存器的（string/slice/small struct）先拷入 gather buffer 再描述。
- **`scatterValue` / `storeResults`**：反方向，把结果写回寄存器或栈槽。string/slice/complex 有专门的拆解路径避免分配。

## 一条完整调用链

```
接口调用 x.M(args)
  → CALL (R6)                     // itab.Fun[k]，裸代码指针，参数已在寄存器/栈
  → 桩（jitcode 生成）             // shuffle：idx 进 R0，receiver 右移，&s0 进溢出位
  → Dispatch                      // 寄存器落位 regBuf，栈参数拷入 stackBuf
  → Invocation.Proceed            // 执行拦截器链
      ├─ fast path: redial        // 寄存器直通 target，零 reflect
      └─ fallback: reflect.Call   // 参数被 materialize 后走反射
  → storeResults                  // 结果 scatter 回寄存器/栈
  → RET                           // 结果原样返回给 caller
```
