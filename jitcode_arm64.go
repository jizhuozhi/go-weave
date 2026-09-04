//go:build arm64

package weave

import (
	"fmt"
	"strconv"
	"strings"
)

// pcQuantum is the minimum instruction size in bytes (sys.PCQuantum).
const pcQuantum = 4

// jitFrameSize is the number of bytes jitStubCode reserves below the incoming
// stack pointer (the SUB immediate in its prologue).
const jitFrameSize = 288

// jitSPDelta is what funcspdelta must report at the call site: the total bytes
// the stack pointer moved from function entry. On arm64 the STP -8(R20) writes
// below the (unmoved) SP, so the delta equals the SUB immediate.
const jitSPDelta = jitFrameSize

// --- mnemonic assembler -----------------------------------------------------
//
// asm parses an AArch64 GNU-syntax instruction string into its 32-bit encoding,
// so the stub body reads like assembly. It understands exactly the instructions
// jitStubCode emits; format verbs substitute immediates and offsets at
// generation time. Each case in parseIns encodes the instruction's fixed bits
// as a base constant with the operand fields shifted into place, with a comment
// naming the field layout.

func asm(format string, args ...any) uint32 {
	return parseIns(fmt.Sprintf(format, args...))
}

func tokenize(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', ',', '[', ']':
			return true
		}
		return false
	})
}

// parseReg maps an AArch64 register name to its number. SP and ZR are both 31;
// the encoding context decides which the field means.
func parseReg(s string) uint32 {
	switch s {
	case "SP", "ZR":
		return 31
	case "FP":
		return 29
	case "LR":
		return 30
	}
	if len(s) >= 2 && s[0] == 'R' {
		if n, err := strconv.Atoi(s[1:]); err == nil && n >= 0 && n <= 30 {
			return uint32(n)
		}
	}
	panic("weave: bad register: " + s)
}

// parseImm reads a #N or $N immediate/offset.
func parseImm(s string) int64 {
	if len(s) < 2 || (s[0] != '$' && s[0] != '#') {
		panic("weave: bad immediate: " + s)
	}
	n, err := strconv.ParseInt(s[1:], 10, 64)
	if err != nil {
		panic("weave: bad immediate: " + s)
	}
	return n
}

// parseShift reads a trailing "LSL #N" and returns the hw field (N/16).
func parseShift(t []string, i int) uint32 {
	if i+1 >= len(t) || t[i] != "LSL" {
		panic("weave: bad shift in instruction")
	}
	return uint32(parseImm(t[i+1])) / 16
}

func parseIns(s string) uint32 {
	t := tokenize(s)
	switch t[0] {
	case "SUB": // 0xD1 | imm12<<10 | Rn<<5 | Rd
		return 0xd1000000 | uint32(parseImm(t[3]))<<10 | parseReg(t[2])<<5 | parseReg(t[1])
	case "ADD": // 0x91 | imm12<<10 | Rn<<5 | Rd
		return 0x91000000 | uint32(parseImm(t[3]))<<10 | parseReg(t[2])<<5 | parseReg(t[1])
	case "MOV": // ORR Rd, XZR, Rs: 0xAA0003E0 | Rm<<16 | Rd
		return 0xaa0003e0 | parseReg(t[2])<<16 | parseReg(t[1])
	case "STR": // 0xF9 | (off/8)<<10 | Rn<<5 | Rt
		return 0xf9000000 | (uint32(parseImm(t[3]))/8)<<10 | parseReg(t[2])<<5 | parseReg(t[1])
	case "STP": // 0xA9 | (off/8 & 0x7f)<<15 | Rt2<<10 | Rn<<5 | Rt
		return 0xa9000000 | uint32((parseImm(t[4])/8)&0x7f)<<15 | parseReg(t[2])<<10 | parseReg(t[3])<<5 | parseReg(t[1])
	case "LDP": // 0xA9 | (off/8 & 0x7f)<<15 | Rt2<<10 | Rn<<5 | Rt
		return 0xa9400000 | uint32((parseImm(t[4])/8)&0x7f)<<15 | parseReg(t[2])<<10 | parseReg(t[3])<<5 | parseReg(t[1])
	case "MOVZ": // 0xD28 | imm16<<5 | Rd
		return 0xd2800000 | uint32(parseImm(t[2]))<<5 | parseReg(t[1])
	case "MOVK": // 0xF28 | hw<<21 | imm16<<5 | Rd
		return 0xf2800000 | parseShift(t, 3)<<21 | uint32(parseImm(t[2]))<<5 | parseReg(t[1])
	case "BLR": // 0xD63F0000 | Rn<<5
		return 0xd63f0000 | parseReg(t[1])<<5
	case "RET":
		return 0xd65f03c0
	}
	panic("weave: unknown instruction: " + s)
}

// jitStubCode emits the trampoline machine code for the given shape — the same
// register shuffle the generated arm64 stubs perform, with an absolute call to
// dispatch via MOVZ/MOVK (BL's ±128MB reach is not enough between a mmap'd page
// and the text segment): load dispatch into R16 and BLR it.
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

	put(asm("SUB R20, SP, #%d", jitFrameSize))   // prologue: open the frame
	put(asm("STP R29, R30, [R20, #-8]"))         // save FP and LR
	put(asm("ADD SP, R20, #0"))                  // MOV SP, R20 (ADD #0: ORR's r31 is ZR, not SP)
	put(asm("SUB R29, SP, #8"))                  // frame pointer
	put(asm("STR R15, [SP, #8]"))                // a15, the 17th int arg
	put(asm("ADD R16, SP, #%d", jitFrameSize+8)) // &s0
	put(asm("STR R16, [SP, #16]"))               // stack param

	// Zero the pointer-holding result words before the first safe point.
	for i := 0; i < sh.retWords; i++ {
		if sh.retPtrs&(1<<uint(i)) != 0 {
			w := sh.argWords + i
			put(asm("STR ZR, [R16, #%d]", w*8))
		}
	}

	// Shift the argument registers up one to free R0 for idx.
	for r := 14; r >= 0; r-- {
		put(asm("MOV R%d, R%d", r+1, r))
	}
	put(asm("MOVZ R0, #%d", sh.index))

	// Load dispatch's 64-bit address into R16 with MOVZ/MOVK and branch to it.
	put(asm("MOVZ R16, #%d", uint16(dispatch)))
	put(asm("MOVK R16, #%d, LSL #16", uint16(dispatch>>16)))
	put(asm("MOVK R16, #%d, LSL #32", uint16(dispatch>>32)))
	put(asm("MOVK R16, #%d, LSL #48", uint16(dispatch>>48)))
	put(asm("BLR R16"))

	put(asm("LDP R29, R30, [SP, #-8]")) // restore FP and LR
	put(asm("ADD SP, SP, #%d", jitFrameSize))
	put(asm("RET"))
	return code[:off]
}
