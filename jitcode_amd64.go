//go:build amd64

package weave

import (
	"fmt"
	"strconv"
	"strings"
)

// pcQuantum is the minimum instruction size in bytes (sys.PCQuantum).
const pcQuantum = 1

// jitFrameSize is the number of bytes jitStubCode reserves below the incoming
// stack pointer (the SUBQ immediate in its prologue).
const jitFrameSize = 0xd0

// jitSPDelta is what funcspdelta must report at the call site: the total bytes
// the stack pointer moved from function entry. The prologue's PUSHQ BP already
// moved SP by 8 before the SUBQ, so the delta is 8 + jitFrameSize.
const jitSPDelta = jitFrameSize + 8

// --- mnemonic assembler -----------------------------------------------------
//
// asm parses an Intel-syntax instruction string into its byte encoding, so the
// stub body reads like assembly. It understands exactly the instructions
// jitStubCode emits; format verbs substitute immediates and displacements.
// x86-64 instructions are variable length, so each parse case builds the byte
// sequence directly: REX prefix, opcode, ModRM, optional SIB and displacement.

func asm(format string, args ...any) []byte {
	return parseIns(fmt.Sprintf(format, args...))
}

func imm32(v int32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }

// dispEncoding chooses the ModRM mod bits and displacement bytes for disp.
func dispEncoding(disp int32) (mod byte, d []byte) {
	switch {
	case disp == 0:
		return 0x00, nil
	case disp >= -128 && disp <= 127:
		return 0x01, []byte{byte(disp)}
	default:
		return 0x02, imm32(disp)
	}
}

func tokenize(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ','
	})
}

// parseReg64 maps a 64-bit register name to its ModRM number.
func parseReg64(s string) byte {
	switch s {
	case "RAX":
		return 0
	case "RCX":
		return 1
	case "RDX":
		return 2
	case "RBX":
		return 3
	case "RSP":
		return 4
	case "RBP":
		return 5
	case "RSI":
		return 6
	case "RDI":
		return 7
	case "R8":
		return 8
	case "R9":
		return 9
	case "R10":
		return 10
	case "R11":
		return 11
	case "R12":
		return 12
	}
	panic("weave: bad register: " + s)
}

// parseMem reads "[base+disp]" or "[base]" into its parts.
func parseMem(s string) (base string, disp int64) {
	inner := strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	if i := strings.IndexByte(inner, '+'); i >= 0 {
		return inner[:i], parseImm("#" + inner[i+1:])
	}
	return inner, 0
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

func isImm(s string) bool { return strings.HasPrefix(s, "#") || strings.HasPrefix(s, "$") }

// parseMov handles MOV dst, src in its three forms: register-register,
// [mem] <- register, and [mem] <- immediate.
func parseMov(t []string) []byte {
	dst, src := t[1], t[2]
	if !strings.HasPrefix(dst, "[") {
		// register-register: REX.W + 0x89 + ModRM(reg=src, r/m=dst)
		rs, rd := parseReg64(src), parseReg64(dst)
		rex := byte(0x48)
		if rs&8 != 0 {
			rex |= 0x04 // REX.R extends reg (src)
		}
		if rd&8 != 0 {
			rex |= 0x01 // REX.B extends r/m (dst)
		}
		return []byte{rex, 0x89, 0xc0 | (rs&7)<<3 | (rd & 7)}
	}

	base, disp := parseMem(dst)
	rb := parseReg64(base)
	mod, d := dispEncoding(int32(disp))
	if isImm(src) {
		// [mem] <- imm: REX.W + 0xC7 + ModRM(reg=0, r/m=base) + [SIB] + disp + imm32
		b := []byte{0x48, 0xc7, mod<<6 | rb}
		if rb&7 == 4 { // RSP addressing goes through the SIB byte
			b = append(b, 0x24)
		}
		b = append(b, d...)
		return append(b, imm32(int32(parseImm(src)))...)
	}

	// [mem] <- register: REX.W + 0x89 + ModRM(reg=src, r/m=base) + [SIB] + disp
	rs := parseReg64(src)
	rex := byte(0x48)
	if rs&8 != 0 {
		rex |= 0x04
	}
	b := []byte{rex, 0x89, mod<<6 | (rs&7)<<3 | rb}
	if rb&7 == 4 {
		b = append(b, 0x24)
	}
	return append(b, d...)
}

// parseLea handles LEA dst, [base+disp].
func parseLea(t []string) []byte {
	dst := parseReg64(t[1])
	base, disp := parseMem(t[2])
	rb := parseReg64(base)
	mod, d := dispEncoding(int32(disp))
	rex := byte(0x48)
	if dst&8 != 0 {
		rex |= 0x04
	}
	b := []byte{rex, 0x8d, mod<<6 | (dst&7)<<3 | rb}
	if rb&7 == 4 {
		b = append(b, 0x24)
	}
	return append(b, d...)
}

func parseIns(s string) []byte {
	t := tokenize(s)
	switch t[0] {
	case "PUSH": // 0x50 | reg
		return []byte{0x50 | parseReg64(t[1])}
	case "POP": // 0x58 | reg
		return []byte{0x58 | parseReg64(t[1])}
	case "SUB": // SUB RSP, imm: 0x48 0x81 0xEC imm32
		return append([]byte{0x48, 0x81, 0xec}, imm32(int32(parseImm(t[2])))...)
	case "ADD": // ADD RSP, imm: 0x48 0x81 0xC4 imm32
		return append([]byte{0x48, 0x81, 0xc4}, imm32(int32(parseImm(t[2])))...)
	case "MOV":
		return parseMov(t)
	case "LEA":
		return parseLea(t)
	case "MOVL": // MOV EAX, imm: 0xB8 imm32
		return append([]byte{0xb8}, imm32(int32(parseImm(t[2])))...)
	case "MOVABS": // MOVABS R12, imm64: 0x49 0xBC imm64
		b := []byte{0x49, 0xbc}
		v := uint64(parseImm(t[2]))
		for i := 0; i < 8; i++ {
			b = append(b, byte(v>>(8*i)))
		}
		return b
	case "CALL": // CALL R12: 0x41 0xFF 0xD4
		return []byte{0x41, 0xff, 0xd4}
	case "RET":
		return []byte{0xc3}
	}
	panic("weave: unknown instruction: " + s)
}

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
	emit := func(b ...[]byte) {
		for _, s := range b {
			code = append(code, s...)
		}
	}

	emit(asm("PUSH RBP"), asm("MOV RBP, RSP"), asm("SUB RSP, #%d", jitFrameSize)) // prologue
	emit(asm("MOV [RSP], R11"))                                                   // a8, dispatch's 9th int arg
	emit(asm("LEA RDX, [RSP+%d]", jitFrameSize+16))                               // &s0
	emit(asm("MOV [RSP+8], RDX"))                                                 // stack param

	// Zero the pointer-holding result words before the first safe point.
	for i := 0; i < sh.retWords; i++ {
		if sh.retPtrs&(1<<uint(i)) != 0 {
			disp := int32(sh.argWords+i) * 8
			emit(asm("MOV [RDX+%d], #0", disp))
		}
	}

	// Shift the argument registers up one to free AX for idx (Intel dst, src).
	for _, ins := range []string{
		"MOV R11, R10", // R10 -> R11
		"MOV R10, R9",  // R9  -> R10
		"MOV R9, R8",   // R8  -> R9
		"MOV R8, RSI",  // SI  -> R8
		"MOV RSI, RDI", // DI  -> SI
		"MOV RDI, RCX", // CX  -> DI
		"MOV RCX, RBX", // BX  -> CX
		"MOV RBX, RAX", // AX  -> BX
	} {
		emit(asm(ins))
	}

	emit(asm("MOVL EAX, #%d", sh.index))   // idx
	emit(asm("MOVABS R12, #%d", dispatch)) // dispatch address
	emit(asm("CALL R12"))

	emit(asm("ADD RSP, #%d", jitFrameSize), asm("POP RBP"), asm("RET"))
	return code
}
