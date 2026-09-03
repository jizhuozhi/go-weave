//go:build amd64

package weave

// pcQuantum is the minimum instruction size in bytes (sys.PCQuantum).
const pcQuantum = 1

// jitFrameSize is the number of bytes jitStubCode reserves below the incoming
// stack pointer (the SUBQ immediate in its prologue).
const jitFrameSize = 0xd0

// jitSPDelta is what funcspdelta must report at the call site: the total bytes
// the stack pointer moved from function entry. The prologue's PUSHQ BP already
// moved SP by 8 before the SUBQ, so the delta is 8 + jitFrameSize.
const jitSPDelta = jitFrameSize + 8

// jitStubCode emits the trampoline machine code for the given shape — the same
// register shuffle the generated amd64 stubs perform (AX,BX,CX,DI,SI,R8..R11),
// with an absolute call to dispatch via MOVABS so the mmap'd page and the text
// segment may be arbitrarily far apart.
//
// The result words the shape marks as pointers are cleared before the call:
// they still hold the caller's old frame contents while the argument map
// already claims they are pointers, so a collection before the first safe point
// would see a stale word through the new map. The stub is pure machine code
// with no safe point until the CALL, so clearing them first is enough.
func jitStubCode(sh stubShape, dispatch uintptr) []byte {
	var code []byte
	emit := func(b ...byte) { code = append(code, b...) }

	emit(0x55)                                  // PUSHQ BP
	emit(0x48, 0x89, 0xe5)                      // MOVQ SP, BP
	emit(0x48, 0x81, 0xec, 0xd0, 0, 0, 0)       // SUBQ $0xd0, SP
	emit(0x4c, 0x89, 0x1c, 0x24)                // MOVQ R11, 0(SP)      (a8, dispatch's 9th int arg)
	emit(0x48, 0x8d, 0x94, 0x24, 0xe0, 0, 0, 0) // LEAQ 0xe0(SP), DX  (&s0)
	emit(0x48, 0x89, 0x54, 0x24, 0x08)          // MOVQ DX, 0x8(SP)     (stack param)

	// Zero the pointer-holding result words before the first safe point.
	for i := 0; i < sh.retWords; i++ {
		if sh.retPtrs&(1<<uint(i)) != 0 {
			disp := (sh.argWords + i) * 8
			emit(0x48, 0xc7, 0x82,
				byte(disp), byte(disp>>8), byte(disp>>16), byte(disp>>24),
				0, 0, 0, 0) // MOVQ $0, disp(DX)
		}
	}

	// Shift the argument registers up one to free AX for idx.
	emit(0x4d, 0x89, 0xd3) // MOVQ R10, R11
	emit(0x4d, 0x89, 0xca) // MOVQ R9, R10
	emit(0x4d, 0x89, 0xc1) // MOVQ R8, R9
	emit(0x49, 0x89, 0xf0) // MOVQ SI, R8
	emit(0x48, 0x89, 0xfe) // MOVQ DI, SI
	emit(0x48, 0x89, 0xcf) // MOVQ CX, DI
	emit(0x48, 0x89, 0xd9) // MOVQ BX, CX
	emit(0x48, 0x89, 0xc3) // MOVQ AX, BX

	emit(0xb8, byte(sh.index), byte(sh.index>>8), byte(sh.index>>16), byte(sh.index>>24)) // MOVL $idx, AX

	emit(0x49, 0xbc) // MOVABS $dispatch, R12
	for i := 0; i < 8; i++ {
		emit(byte(dispatch >> (8 * i)))
	}
	emit(0x41, 0xff, 0xd4) // CALL R12

	emit(0x48, 0x81, 0xc4, 0xd0, 0, 0, 0) // ADDQ $0xd0, SP
	emit(0x5d)                            // POPQ BP
	emit(0xc3)                            // RET
	return code
}
