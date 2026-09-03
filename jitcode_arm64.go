//go:build arm64

package weave

// pcQuantum is the minimum instruction size in bytes (sys.PCQuantum).
const pcQuantum = 4

// jitFrameSize is the number of bytes jitStubCode reserves below the incoming
// stack pointer (the SUB immediate in its prologue). funcspdelta must report
// exactly this at the call site or the runtime's unwind fails.
const jitFrameSize = 288

// jitStubCode emits the trampoline machine code for method index idx — the same
// register shuffle the generated arm64 stubs perform, with an absolute call to
// dispatch (BL's ±128MB reach is not enough between a mmap'd page and the text
// segment): load dispatch into R16 with MOVZ/MOVK and BLR it.
func jitStubCode(idx int, dispatch uintptr) []byte {
	off := 0
	code := make([]byte, 128)
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
	for r := 14; r >= 0; r-- {
		put(0xaa0003e0 | uint32(r)<<16 | uint32(r+1)) // MOVD R{r}, R{r+1}
	}
	put(0xd2800000 | uint32(idx)<<5) // MOVZ $idx, R0
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
