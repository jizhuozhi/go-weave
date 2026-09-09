# 接口调用与 itab

本库的核心动作是在运行时伪造一个 `itab`，使从未声明"实现了 T"的类型 `*Proxy` 成为一个合法的接口值 `T`。本文说明接口值、`itab` 的布局，以及方法表为何必须存放裸代码指针。这是整个机制的起点。

## 接口值 = 两个词

非空接口值在内存中是两个机器字（`rt.go`）：

```go
type iface struct {
    tab  *itab          // 分发表
    data unsafe.Pointer // 底层具体值
}

type eface struct {     // 空接口 any
    typ  *abiType
    data unsafe.Pointer
}
```

注意非空接口的首字是 `*itab`，而 `any` 的首字是 `*abiType`。二者不能混用——这也是组装接口值时不能经 `any` 参数中转的原因（`rt.go` 中 `makeIface` 的注释说明了这一点：Go 会在进入 `any` 参数时把 `*itab` 转换为 `*abiType`，破坏分发表）。

> **并发小贴士**：`itab + data` 两个字即俗称的胖指针（fat pointer）。由于接口值占两个字，赋值需要两次 store，编译器不会合成一次原子 16 字节写，因此接口赋值是非原子的——两个 goroutine 并发读写同一个接口值，可能产生 `tab` 与 `data` 不属于同一时刻的不一致组合。

## itab 的布局

```go
type itab struct {
    Inter *interfaceType // 接口类型
    Type  *abiType       // 具体类型
    Hash  uint32         // Type.Hash 的副本，用于 type switch
    _     [4]byte
    Fun   [1]uintptr     // 实际是可变长数组：每个方法一个裸代码指针
}
```

`Fun` 声明为长度 1，但真实 `itab` 之后跟着 `n` 个 `uintptr`，每个是一个方法的入口代码指针，按接口方法排序（与 `reflect.Type.Method(i)` 一致）。

## 接口分派：为什么必须是裸代码指针

编译器对接口调用 `x.M()` 生成的机器码（arm64 上大致为）：

```
MOVD 24(itab), R6   // 取 Fun[k] 的代码指针
CALL (R6)           // 间接调用，不传递任何闭包上下文
```

`24` 是 `Fun` 数组在 `itab` 里的字节偏移：`Inter`（8）+ `Type`（8）+ `Hash`（4）+ padding（4）= 24。方法下标 `k` 在编译期已知，所以是 `24 + 8*k` 处取指针。

关键约束在于 **`CALL (R6)` 不携带闭包上下文**，这排除了两种候选：

- **闭包**：闭包入口期望从闭包寄存器读取上下文，但接口调用不提供；
- **`reflect.MakeFunc`**：其入口 `makeFuncStub` 同样期望上下文在闭包寄存器。

因此 `Fun[k]` 只能是裸代码指针——普通函数，或运行时生成的机器码（本库采用后者，见 [05-jit.md](05-jit.md)）。

## 伪造 itab

`forgeITab`（`rt.go`）在 `[]unsafe.Pointer` 中构造 itab 的三个头字与 n 个方法指针：

```go
func forgeITab(inter *interfaceType, proxyType *abiType, funs []unsafe.Pointer) *itab {
    n := len(inter.Methods)
    w := make([]unsafe.Pointer, 3+n)
    w[0] = unsafe.Pointer(inter)
    w[1] = unsafe.Pointer(proxyType)
    *(*uint32)(unsafe.Pointer(&w[2])) = proxyType.Hash // Hash 与 4 字节 padding 共用一字
    for i, f := range funs {
        w[3+i] = f
    }
    return (*itab)(unsafe.Pointer(&w[0]))
}
```

**为何用 `[]unsafe.Pointer` 而非裸内存**：runtime 用 `persistentalloc` 分配真实 itab（永不回收、GC 不扫描），而伪造的 itab 中 `Inter`/`Type`/`Fun` 全部是引用（方法桩、类型描述符）。放入 `[]unsafe.Pointer`，GC 即可扫描这些引用，确保它们不被回收。

## 组装接口值

`makeIface`（`rt.go`）将伪造的 itab 与 data 指针直接写入 `*T` 的存储：

```go
func makeIface[T any](dst *T, tab *itab, data unsafe.Pointer) {
    i := (*iface)(unsafe.Pointer(dst))
    i.tab = tab
    i.data = data
}
```

`NewOf` 中，`Type` 字段记录的是 `*Proxy`（`typeOf(reflect.TypeOf(p))`），与 data 指针指向的对象一致，因此 GC 能正确扫描 `*Proxy` 的字段。

## init 自检

`itab` 的布局没有任何兼容性承诺，`rt.go` 的 `init` 用一个真实 itab（`&itabProbe{}` 赋给 `itabProbeIface`）校验三个关键 offset：

- `itab.Inter` 落在 interface-kind 类型描述符上（校验 itab 头与 `interfaceType` 的嵌入）；
- `itab.Hash == itab.Type.Hash`（校验两个 `Hash` 字段）；
- `itab.Fun[0]` 指向方法自身入口（校验 `Fun` 偏移）。

任一不符即 panic，报出不支持的 Go 版本，而非在运行时静默破坏内存。
