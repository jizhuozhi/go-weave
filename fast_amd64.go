//go:build amd64

package weave

import (
	"reflect"
	"unsafe"
)

// fastTrampoline reports whether this build ships the fast trampoline.
const fastTrampoline = true

// The trampoline has a maximal register signature: nine integer registers
// (AX BX CX DI SI R8 R9 R10 R11) and fifteen floating point registers
// (X0-X14). Each slot in stubs_gen_amd64.go is its own Go function, so the
// compiler emits a distinct code pointer per slot — a bare pointer with no
// closure context, which is exactly what itab.Fun[k] expects.
type fastFunc = func(a0, a1, a2, a3, a4, a5, a6, a7, a8 uintptr,
	f0, f1, f2, f3, f4, f5, f6, f7, f8, f9, f10, f11, f12, f13, f14 float64,
	s0 [stackWindow]byte) (
	r0, r1, r2, r3, r4, r5, r6, r7, r8 unsafe.Pointer,
	g0, g1, g2, g3, g4, g5, g6, g7, g8, g9, g10, g11, g12, g13, g14 float64)

// methodSlots maps a trampoline slot to its method. Slots are assigned once per
// distinct method and never released.
// methodSlots used to map a slot to a *Method assigned by a global counter,
// which capped the number of distinct methods process-wide. The slot index is
// now simply the method's index in the interface, and dispatch resolves the
// *Method through the receiver's proxy, so every interface shares the same
// slots and only the methods-per-interface count is bounded.

// fastStub returns the code pointer for method index i of any interface.
func fastStub(i int) unsafe.Pointer {
	if uint(i) >= uint(slotCount) {
		panic("weave: interface has more methods than trampoline slots; raise gen/main.go's slots constant and run go generate")
	}
	return unsafe.Pointer(reflect.ValueOf(stubs[i]).Pointer())
}

// redial, implemented in redial_amd64.s, reloads the argument registers from
// regs, calls fun (a bare ABIInternal code pointer, an itab.Fun entry) and
// stores the result registers back into regs. The method must pass nothing on
// the stack, which fitsFastPath guarantees.
func redial(fun unsafe.Pointer, regs *regBuf)

// Dispatch runs the interceptor chain on the raw register contents and returns
// the results in the same register positions. The receiver is the first
// integer register, which is how interface calls pass it.
//
// Dispatch is exported only so that the precise trampolines StubSource
// generates — which live in the caller's own package — can reach it. Its
// signature is the architecture's register file and changes with the
// architecture; nothing outside generated code should call it.
//
//go:nocheckptr
func Dispatch(idx int, a0, a1, a2, a3, a4, a5, a6, a7, a8 uintptr,
	f0, f1, f2, f3, f4, f5, f6, f7, f8, f9, f10, f11, f12, f13, f14 float64,
	stack unsafe.Pointer) (
	r0, r1, r2, r3, r4, r5, r6, r7, r8 unsafe.Pointer,
	g0, g1, g2, g3, g4, g5, g6, g7, g8, g9, g10, g11, g12, g13, g14 float64) {

	// idx is the method's index in the interface, which is also its index in
	// itab.Fun. The receiver resolves the concrete *Method — including which
	// proxy's target to delegate to — so one trampoline slot serves method idx
	// of every interface simultaneously.

	// Mirror the pointer registers into GC-visible storage before any call:
	// pool.Get may allocate, and until then pointer arguments are only live
	// in unscanned uintptr slots.
	ints := [intArgRegs]uintptr{a0, a1, a2, a3, a4, a5, a6, a7, a8}
	p := (*Proxy)(unsafe.Pointer(ints[0]))
	m := p.methods[idx]
	mask := m.layout.ptrMask
	// Zero first: the array is typed unsafe.Pointer, so the collector and
	// copystack treat every word as a pointer, and unwritten slots would
	// hold stack garbage.
	prePtrs := [intArgRegs]unsafe.Pointer{}
	for i := 0; i < intArgRegs; i++ {
		if mask&(1<<uint(i)) != 0 {
			prePtrs[i] = unsafe.Pointer(ints[i])
		}
	}

	st := statePool.Get().(*callState)

	regs := &st.regs
	regs.ints = ints
	regs.floats = [floatArgRegs]float64{f0, f1, f2, f3, f4, f5, f6, f7, f8, f9, f10, f11, f12, f13, f14}
	regs.ptrs = prePtrs

	// Stack-assigned arguments: copy them out of the caller's outgoing area
	// into the pooled buffer. The raw stack pointer must not be stored in
	// the heap Invocation — a stack move inside an interceptor would leave
	// it dangling — while this plain local is adjusted by copystack. The
	// area is pointer-free by construction (layouts with pointer-bearing
	// stack arguments are rejected), so the copy needs no GC mirror.
	var stk unsafe.Pointer
	if m.layout.stackBytes != 0 {
		copy(st.stackBuf[:m.layout.stackCallArgsSize],
			unsafe.Slice((*byte)(stack), m.layout.stackCallArgsSize))
		stk = unsafe.Pointer(&st.stackBuf[0])
	}

	c := &st.inv
	*c = Invocation{Proxy: p, Method: m, chain: p.chain, regs: regs, snap: &st.saved, stack: stk}
	c.storeResults(c.Proceed())

	if m.layout.stackBytes != 0 {
		// Copy the stack-assigned results from the buffer back into the
		// caller's outgoing area.
		l := m.layout
		n := l.stackBytes - l.retOffset
		copy(unsafe.Slice((*byte)(add(stack, l.retOffset)), n), st.stackBuf[l.retOffset:l.stackBytes])
	}

	// No defer on the Put, and no call after this point: the return values
	// below are unsafe.Pointers converted from raw uintptr results, and
	// while they are spilled in this frame the collector must not see them.
	ri, rf := regs.ints, regs.floats
	statePool.Put(st)

	return unsafe.Pointer(ri[0]), unsafe.Pointer(ri[1]), unsafe.Pointer(ri[2]),
		unsafe.Pointer(ri[3]), unsafe.Pointer(ri[4]),
		unsafe.Pointer(ri[5]), unsafe.Pointer(ri[6]), unsafe.Pointer(ri[7]),
		unsafe.Pointer(ri[8]),
		rf[0], rf[1], rf[2], rf[3], rf[4],
		rf[5], rf[6], rf[7], rf[8], rf[9],
		rf[10], rf[11], rf[12], rf[13], rf[14]
}
