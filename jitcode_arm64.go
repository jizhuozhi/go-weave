//go:build arm64

package weave

// pcQuantum is the minimum instruction size in bytes (sys.PCQuantum).
const pcQuantum = 4

// jitFrameSize is the number of bytes jitStubCode reserves below the incoming
// stack pointer (the SUB immediate in its prologue).
const jitFrameSize = 288

// jitSPDelta is what funcspdelta must report at the call site: the total bytes
// the stack pointer moved from function entry. On arm64 the STP -8(R20) writes
// below the (unmoved) SP, so the delta equals the SUB immediate.
const jitSPDelta = jitFrameSize

// jitStubCode emits the trampoline machine code for the given shape — the same
// register shuffle the generated arm64 stubs perform, with an absolute call to
// dispatch (BL's ±128MB reach is not enough between a mmap'd page and the text
// segment): load dispatch into R16 with MOVZ/MOVK and BLR it.
//
// The result words the shape marks as pointers are cleared before the call:
// they still hold the caller's old frame contents while the argument map
// already claims they are pointers, so a collection before the first safe point
// would see a stale word through the new map. The stub is pure machine code
// with no safe point until BLR, so clearing them first is enough.
func jitStubCode(sh stubShape, dispatch uintptr) []byte {
	off := 0
	code := make([]byte, 512)
	put := func(ins uint32) {
		code[off] = byte(ins)
		code[off+1] = byte(ins >> 8)
		code[off+2] = byte(ins >> 16)
		code[off+3] = byte(ins >> 24)
		off += 4
	}
	put(0xd10483f4) // SUB $288, RSP, R20
	put(0xa93ffa9d) // STP (R29, R30), -8(R20)
	put(0x9100029f) // MOVD R20, RSP
	put(0xd10023fd) // SUB $8, RSP, R29
	put(0xf90007ef) // MOVD R15, 8(RSP)
	put(0x9104a3f0) // ADD $296, RSP, R16  (&s0)
	put(0xf9000bf0) // MOVD R16, 16(RSP)
	// Zero the pointer-holding result words before the first safe point.
	for i := 0; i < sh.retWords; i++ {
		if sh.retPtrs&(1<<uint(i)) != 0 {
			w := sh.argWords + i
			put(0xf9000000 | uint32(w)<<10 | 16<<5 | 31) // MOVD ZR, (w*8)(R16)
		}
	}
	for r := 14; r >= 0; r-- {
		put(0xaa0003e0 | uint32(r)<<16 | uint32(r+1)) // MOVD R{r}, R{r+1}
	}
	put(0xd2800000 | uint32(sh.index)<<5) // MOVZ $idx, R0
	// Load dispatch's 64-bit address into R16 and branch to it.
	put(0xd2800000 | uint32(uint16(dispatch))<<5 | 16)     // MOVZ X16, #lo
	put(0xf2a00000 | uint32(uint16(dispatch>>16))<<5 | 16) // MOVK X16, #hi16, LSL#16
	put(0xf2c00000 | uint32(uint16(dispatch>>32))<<5 | 16) // MOVK X16, #hi32, LSL#32
	put(0xf2e00000 | uint32(uint16(dispatch>>48))<<5 | 16) // MOVK X16, #hi48, LSL#48
	put(0xd63f0200)                                        // BLR X16
	put(0xa97ffbfd)                                        // LDP -8(RSP), (R29, R30)
	put(0x910483ff)                                        // ADD $288, RSP, RSP
	put(0xd65f03c0)                                        // RET
	return code[:off]
}
