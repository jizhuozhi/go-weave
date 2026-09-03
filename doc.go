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
// Instead each interface method index gets one dedicated trampoline, generated
// at runtime (see "Runtime JIT" below): the 128 generic slots are prefetched at
// startup, and pointer-spilling shapes are compiled at proxy construction.
// Every slot has the architecture's maximal register signature and forwards to
// the dispatcher with its own hardcoded slot index — which is also the method's
// index in the interface, so the receiver alone resolves which method runs, and
// every interface shares the same slots.
//
// The trampoline is pure machine code, so two hazards the generated Go stubs
// used to need directives for now hold for free. A register parameter's ABI
// home slot lives in the caller's outgoing argument area, which an itab call
// site reserves only for the method's own homes:
//
//   - there is no race prologue to spill all parameters across the racefuncenter
//     call into the unreserved homes;
//   - there is no stack check, so no morestack trampoline saves every argument
//     register past the itab caller's reservation.
//
// The trampoline is a pure register conduit with no memory accesses beyond the
// result-word clearing, so excluding it from race instrumentation costs
// nothing: from dispatch down, everything stays fully instrumented.
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
// The way out is a trampoline that is not generic, generated at runtime (see
// "Runtime JIT" below).
//
// # Runtime JIT
//
// At proxy construction the proxy mmaps an executable page, writes the
// trampoline's machine code — the same register shuffle the generic stubs
// perform — and builds a moduledata whose argument pointer map marks exactly
// the pointer words of the stack area (argument words then result words). The
// module is appended to runtime.lastmoduledatap via linkname, so findfunc
// recognises the injected code and the collector scans every pointer in the
// area from function entry onwards — there is no window at all. The cost is a
// one-off per shape, cached.
//
// Because the result words still hold whatever the caller's frame held before
// the call while the new pointer map already claims them, the trampoline clears
// the pointer-holding ones before its first call — it is pure machine code with
// no safe point until then, so the collector never sees a stale word.
//
// Methods whose pointers stay inside the register file — nearly all of them —
// need no generated code and keep using the generic trampoline.
//
// The forged moduledata layout is version-specific, so the mirror is split
// across the Go versions that changed it (see moduledata_go1*.go); each
// version segment is guarded by CI.
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
// worth remembering: an interface travels through the ABI as (itab, data),
// while reflect's own representation of a value begins with (type, data) — so
// materializeIface and scatterIface reassemble interfaces in both directions
// rather than copying them like plain memory. The first word of an interface
// is not scanned as a pointer by the collector (itab entries are runtime-cached
// and survive on their own), which is why the register pointer mask marks only
// the data word.
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
//     a trampoline that describes that area to the collector, generated at
//     runtime (see "Runtime JIT" above). Since register assignment is
//     positional, moving pointer arguments to the front of a signature is
//     often enough to avoid the stack area altogether.
//   - There are 128 trampoline slots, one per interface method index, so an
//     interface may have at most 128 methods; the number of interfaces and
//     proxies is unbounded. Raise slotCount for more.
//   - The proxy passes as T end to end, but `x.(T)` after converting the proxy
//     to `any` fails: that assertion goes through getitab, which requires the
//     concrete type to statically implement T.
//   - The package pokes at unexported runtime data structures through unsafe.
//     The layouts it depends on (abi.Type, abi.InterfaceType, the itab) have
//     been stable since Go 1.18 introduced the register ABI on arm64, and an
//     init-time self-check validates every offset against a real, runtime
//     built itab — so a future layout change fails at startup with a clear
//     panic instead of corrupting memory. The forged moduledata and _func
//     layouts are version-split across every minor version from Go 1.18
//     through 1.27, guarded by CI.
package weave
