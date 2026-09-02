// Package weave implements runtime dynamic proxies for Go interfaces, the
// closest analogue to java.lang.reflect.Proxy that the Go runtime allows.
//
// # Why this is supposed to be impossible
//
// Go has no class loader, no bytecode and no VM: interfaces are compiled down
// to a two-word value
//
//	iface{ tab *itab, data unsafe.Pointer }
//
// where itab is the dispatch table:
//
//	type itab struct {
//	    Inter *interfacetype
//	    Type  *_type
//	    Hash  uint32
//	    _     [4]byte
//	    Fun   [1]uintptr // variable sized array of code pointers
//	}
//
// The compiler emits a plain indirect call `CALL itab.Fun[k]` for `x.M()`, so
// the only thing that stands between us and a dynamic proxy is that Go refuses
// to build an itab for a type that does not statically implement the
// interface. We simply do not ask: we allocate the itab ourselves, point
// Fun[k] at trampolines of our choosing, and hand the resulting iface to the
// caller. That is the whole trick, and it is done in forgeITab.
//
// # Trampolines
//
// itab.Fun[k] must hold a bare code pointer: the compiler's dispatch sequence
// is `MOVD 24(itab), R6; CALL (R6)` and passes no closure context. That rules
// out closures (their entry expects a context in the closure register) and
// rules out reflect.MakeFunc (whose entry, makeFuncStub, expects the same).
//
// Instead each interface method index gets one dedicated, ordinary Go
// function, generated at build time by gen and checked into
// stubs_gen_arm64.go / stubs_gen_amd64.go. Every slot has the architecture's
// maximal register signature and forwards to the dispatcher with its own
// hardcoded slot index — which is also the method's index in the interface,
// so the receiver alone resolves which method runs, and every interface
// shares the same slots.
//
// The stubs carry two load-bearing directives, both defending against the
// same hazard: a register parameter's ABI home slot lives in the caller's
// outgoing argument area, which a direct call to the stub's signature would
// reserve for all 32 parameters — but an itab call site is compiled against
// the method's signature and reserves only the method's own homes.
//
//   - //go:norace: the race prologue would spill all parameters across the
//     racefuncenter call, straight into the unreserved homes.
//   - //go:nosplit: a split function's morestack trampoline saves every
//     argument register to its home before growing the stack, writing ~250
//     bytes past the itab caller's reservation. nosplit removes the stack
//     check; the ~300 byte frame is well under the StackNosplit limit.
//
// The stub is a pure register conduit with no memory accesses, so excluding
// it from race instrumentation costs nothing: from dispatch down, everything
// stays fully instrumented.
//
// # GC safety of the register spill
//
// The trampoline's integer parameters carry the raw argument registers, most
// of which are garbage for any given signature. Typing them unsafe.Pointer
// makes the compiler mark the spill area as pointers, and the collector then
// aborts with "found bad pointer in Go heap" the first time a garbage word
// happens to look like an address in an unused span region. Typing them
// uintptr makes the collector ignore them — and then it can free a pointer
// argument that is still live only in a register.
//
// The resolution mirrors runtime.RegArgs: the parameters are uintptr and are
// never scanned, and the dispatcher's first order of business is to copy the
// registers that the method's ABI layout marks as pointer-holding (see
// abiLayout.ptrMask) into a parallel unsafe.Pointer array that the collector
// does scan, before the first allocation.
//
// # Pointers through the stack argument area
//
// Arguments and results that spill past the register file land in the caller's
// stack argument area, whose GC description belongs to the callee — the
// trampoline. The generic trampoline describes it as one [stackWindow]byte
// parameter, which is a lie the moment a pointer crosses it: the collector is
// told the area holds no pointers, and no Go code can correct that in time,
// since the correction would have to precede the trampoline's first
// instruction. Typing the window as pointers instead is no better — a
// stack-assigned integer that happens to look like an address then aborts the
// collector.
//
// The way out is a trampoline that is not generic. StubSource emits, for the
// interfaces a program actually proxies, one function per stack-area shape
// whose trailing parameters spell that area out word by word: unsafe.Pointer
// where the method puts a pointer, uintptr elsewhere. The compiler then emits
// an exactly correct argument pointer map, and every pointer in the area is
// visible to the collector from function entry onwards — there is no window at
// all. Generated init functions hand the code pointers to RegisterStub, and
// newTrampoline prefers a registered precise trampoline over the generic one.
//
// Two details make it work. The whole area, results included, is declared as
// parameters rather than results: a declared result belongs to the compiler,
// which zeroes it on entry and may write it back on return, overwriting what
// the dispatcher stored through the area pointer. And because the result words
// then still hold whatever the caller's frame held before the call while the
// new pointer map already claims them, the trampoline clears the
// pointer-holding ones before its first call — a nosplit function has no safe
// point until then, so the collector never sees a stale word.
//
// Methods whose pointers stay inside the register file — nearly all of them —
// need no generated code and keep using the generic trampoline.
//
// # Decoding and encoding arguments
//
// abi.go contains a private copy of Go's register assignment algorithm. It
// computes, per method, which register or stack slot holds which part of which
// argument. An argument that lives in a single register or stack slot is
// described in place with reflect.NewAt, with no copy at all. An argument
// spread over several registers (strings, slices, small structs) is gathered
// into a temporary []byte; that buffer is deliberately untyped so the
// collector ignores it, which is safe because every byte in it is a copy of
// data that is still live in the register spill area or in the caller's frame.
//
// Results are scattered back into the same register positions. One subtlety
// worth remembering: an interface result must be written as (itab, data), not
// as the (type, data) pair that reflect.Value.Interface returns.
//
// # Calling the target
//
// When no interceptor has materialised the arguments, the target is invoked
// without reflect at all: the redial assembly helper reloads the captured
// argument registers, rebinds the receiver to the target (its real itab's
// Fun entry and data word, extracted once per proxy) and calls the target's
// method code directly, so the results land in the result registers by the
// ABI itself. A register snapshot with a GC-visible pointer mirror keeps the
// arguments observable and replayable afterwards. The helper's frame follows
// the reflectcall pattern: sized for the callee's register-argument home
// slots and declared pointer-free for its own locals, since the callee's
// spills are adjusted by the callee's argument maps.
//
// # Limitations
//
//   - Arguments and results that spill past the register file into the
//     caller's stack argument area are supported up to 480 bytes per method
//     (stackWindow). When the spilled part contains pointers the method needs
//     a precise trampoline generated by StubSource; without one it is
//     rejected at proxy construction, with a message naming the shape. Since
//     register assignment is positional, moving pointer arguments to the
//     front of a signature is often enough to avoid the generation step
//     altogether.
//   - There are 128 trampoline slots, one per interface method index, so an
//     interface may have at most 128 methods; the number of interfaces and
//     proxies is unbounded. Raise gen/main.go's slots constant and run
//     `go generate` for more.
//   - The proxy passes as T end to end, but `x.(T)` after converting the proxy
//     to `any` fails: that assertion goes through getitab, which requires the
//     concrete type to statically implement T.
//   - The package pokes at unexported runtime data structures through unsafe.
//     The layouts it depends on (abi.Type, abi.InterfaceType, the itab) have
//     been stable since Go 1.18 introduced the register ABI on arm64, and an
//     init-time self-check validates every offset against a real, runtime
//     built itab — so a future layout change fails at startup with a clear
//     panic instead of corrupting memory. The compatibility range is Go
//     1.18 through 1.24, guarded by CI across every minor version.
package weave

//go:generate go run ./gen
