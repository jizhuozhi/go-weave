//go:build arm64

package weave

import (
	"reflect"
	"unsafe"
)

// fastTrampoline reports whether this build ships the fast trampoline.
const fastTrampoline = true

// The trampoline has a maximal register signature: sixteen integer registers
// (R0-R15) and sixteen floating point registers (F0-F15). Each slot in
// stubs_gen_arm64.go is its own Go function, so the compiler emits a distinct
// code pointer per slot — a bare pointer with no closure context, which is
// exactly what itab.Fun[k] expects.
type fastFunc = func(a0, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14, a15 uintptr,
	f0, f1, f2, f3, f4, f5, f6, f7, f8, f9, f10, f11, f12, f13, f14, f15 float64) (
	r0, r1, r2, r3, r4, r5, r6, r7, r8, r9, r10, r11, r12, r13, r14, r15 unsafe.Pointer,
	g0, g1, g2, g3, g4, g5, g6, g7, g8, g9, g10, g11, g12, g13, g14, g15 float64)

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

// redial, implemented in redial_arm64.s, reloads the argument registers from
// regs, calls fun (a bare ABIInternal code pointer, an itab.Fun entry) and
// stores the result registers back into regs. The method must pass nothing on
// the stack, which fitsFastPath guarantees.
func redial(fun unsafe.Pointer, regs *regBuf)

// dispatch runs the interceptor chain on the raw register contents and returns
// the results in the same register positions. The receiver is the first
// integer register, which is how interface calls pass it.
//
//go:nocheckptr
func dispatch(idx int, a0, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14, a15 uintptr,
	f0, f1, f2, f3, f4, f5, f6, f7, f8, f9, f10, f11, f12, f13, f14, f15 float64) (
	r0, r1, r2, r3, r4, r5, r6, r7, r8, r9, r10, r11, r12, r13, r14, r15 unsafe.Pointer,
	g0, g1, g2, g3, g4, g5, g6, g7, g8, g9, g10, g11, g12, g13, g14, g15 float64) {

	// idx is the method's index in the interface, which is also its index in
	// itab.Fun. The receiver resolves the concrete *Method — including which
	// proxy's target to delegate to — so one trampoline slot serves method idx
	// of every interface simultaneously.

	// Mirror the pointer registers into GC-visible storage before any call:
	// pool.Get may allocate, and until then pointer arguments are only live
	// in unscanned uintptr slots.
	ints := [intArgRegs]uintptr{a0, a1, a2, a3, a4,
		a5, a6, a7, a8, a9, a10, a11,
		a12, a13, a14, a15}
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
	regs.floats = [floatArgRegs]float64{f0, f1, f2, f3, f4, f5, f6, f7, f8, f9, f10, f11, f12, f13, f14, f15}
	regs.ptrs = prePtrs

	c := &st.inv
	*c = Invocation{Proxy: p, Method: m, chain: p.chain, regs: regs, snap: &st.saved}
	c.storeResults(c.Proceed())

	// No defer on the Put, and no call after this point: the return values
	// below are unsafe.Pointers converted from raw uintptr results, and
	// while they are spilled in this frame the collector must not see them.
	ri, rf := regs.ints, regs.floats
	statePool.Put(st)

	return unsafe.Pointer(ri[0]), unsafe.Pointer(ri[1]), unsafe.Pointer(ri[2]),
		unsafe.Pointer(ri[3]), unsafe.Pointer(ri[4]), unsafe.Pointer(ri[5]),
		unsafe.Pointer(ri[6]), unsafe.Pointer(ri[7]), unsafe.Pointer(ri[8]),
		unsafe.Pointer(ri[9]), unsafe.Pointer(ri[10]), unsafe.Pointer(ri[11]),
		unsafe.Pointer(ri[12]), unsafe.Pointer(ri[13]), unsafe.Pointer(ri[14]),
		unsafe.Pointer(ri[15]),
		rf[0], rf[1], rf[2], rf[3], rf[4], rf[5],
		rf[6], rf[7], rf[8], rf[9], rf[10], rf[11],
		rf[12], rf[13], rf[14], rf[15]
}
