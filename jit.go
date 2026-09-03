//go:build amd64 || arm64

package weave

// Runtime JIT: generate every trampoline's machine code and a moduledata whose
// argument pointer map describes the method's stack argument area. There is no
// compile-time codegen at all — the generic trampolines are prefetched at
// startup, and pointer-spilling trampolines are compiled at proxy construction
// (one per shape, cached). The trampoline itself is a plain register shuffle;
// only the moduledata is shaped per method, and the argument pointer map is
// built directly rather than derived from a compiled function signature.
//
// A generated module is appended to runtime.lastmoduledatap (via linkname) so
// findfunc recognises it and the collector scans its frame. Once registered it
// must live for the process lifetime: findfunc walks that list on every stack
// scan, so the page and every allocation the moduledata reaches are rooted in
// jitRoots and never freed.

import (
	"reflect"
	"sync"
	"unsafe"
	_ "unsafe" // for //go:linkname
)

//go:linkname lastmoduledatap runtime.lastmoduledatap
var lastmoduledatap *jitModuledata

// slotCount is the number of generic trampoline slots, one per interface method
// index: slot k serves method k of every interface simultaneously, so the bound
// is methods-per-interface, not process-wide.
const slotCount = 128

// jitMu serialises registration against concurrent proxy construction.
var jitMu sync.Mutex

// jitBySh caches the generated trampoline for each stack-area shape, so a shape
// is compiled once and every proxy using it shares the code.
var jitBySh = map[stubShape]unsafe.Pointer{}

// jitRoots keeps heap allocations referenced only through uintptr fields of the
// (never-scanned-by-construction) moduledata reachable from a GC root.
var jitRoots []any

// jitStubs holds the prefetched generic trampolines, generated at startup so the
// hot path never pays a JIT cost. Slot k dispatches method index k; its stack
// area holds no pointers, so the argument map is one pointer-free window.
var jitStubs [slotCount]unsafe.Pointer

func init() {
	// Prefetch the generic trampolines up front — a predictable one-off startup
	// cost, like a pretouch, instead of a per-slot compile on the first call.
	for i := 0; i < slotCount; i++ {
		sh := stubShape{index: i, argWords: stackWindow / int(ptrSize)}
		if jitStubs[i] = jitTrampoline(sh); jitStubs[i] == nil {
			panic("weave: runtime JIT failed to generate a generic trampoline")
		}
	}
}

// --- runtime layout mirrors ------------------------------------------------
//
// These mirror Go 1.23+ runtime layouts. The field order and types are
// load-bearing; TestJITFindfunc verifies the offsets that matter (text, pctab,
// gofunc, next) against the values the runtime actually uses.

// pcHeader mirrors runtime.pcHeader.
type pcHeader struct {
	magic          uint32
	pad1, pad2     uint8
	minLC          uint8
	ptrSize        uint8
	nfunc          int
	nfiles         uint
	textStart      uintptr
	funcnameOffset uintptr
	cuOffset       uintptr
	filetabOffset  uintptr
	pctabOffset    uintptr
	pclnOffset     uintptr
}

// functab mirrors runtime.functab.
type functab struct {
	entryoff uint32
	funcoff  uint32
}

// findfuncbucket mirrors runtime.findfuncbucket.
type findfuncbucket struct {
	idx        uint32
	subbuckets [16]byte
}

// rfunc mirrors runtime._func, minus the trailing variable arrays.
type rfunc struct {
	entryOff    uint32
	nameOff     int32
	args        int32
	deferreturn uint32
	pcsp        uint32
	pcfile      uint32
	pcln        uint32
	npcdata     uint32
	cuOffset    uint32
	startLine   int32
	funcID      uint8
	flag        uint8
	_           [1]byte
	nfuncdata   uint8
}

// stackmap mirrors runtime.stackmap.
type stackmap struct {
	n        int32
	nbit     int32
	bytedata [1]byte
}

// textsect mirrors runtime.textsect.
type textsect struct {
	vaddr, end, baseaddr uintptr
}

// ptabEntry mirrors runtime.ptabEntry.
type ptabEntry struct {
	name nameOff
	typ  typeOff
}

// modulehash mirrors runtime.modulehash.
type modulehash struct {
	modulename   string
	linktimehash string
	runtimehash  *string
}

// initTask is an opaque placeholder; only the slice field's size matters.
type initTask struct{ _ [0]byte }

// bitvector mirrors runtime.bitvector.
type bitvector struct {
	n        int32
	bytedata *byte
}

// nameOff / typeOff are runtime offsets; the map value type is irrelevant for
// layout, so aliasing uintptr keeps the field sized identically.
type nameOff = int32
type typeOff = uint32

// jitModuledata mirrors runtime.moduledata (Go 1.23+).
type jitModuledata struct {
	pcHeader     *pcHeader
	funcnametab  []byte
	cutab        []uint32
	filetab      []byte
	pctab        []byte
	pclntable    []byte
	ftab         []functab
	findfunctab  uintptr
	minpc, maxpc uintptr

	text, etext           uintptr
	noptrdata, enoptrdata uintptr
	data, edata           uintptr
	bss, ebss             uintptr
	noptrbss, enoptrbss   uintptr
	covctrs, ecovctrs     uintptr
	end, gcdata, gcbss    uintptr
	types, etypes         uintptr
	rodata                uintptr
	gofunc                uintptr

	textsectmap []textsect
	typelinks   []int32
	itablinks   []*itab

	ptab []ptabEntry

	pluginpath string
	pkghashes  []modulehash

	inittasks []*initTask

	modulename   string
	modulehashes []modulehash

	hasmain uint8
	bad     bool

	gcdatamask, gcbssmask bitvector

	typemap map[typeOff]*abiType

	next *jitModuledata
}

// pcdata / funcdata table indexes (mirror internal/abi).
const (
	pcdUnsafePoint   = 0
	pcdStackMapIndex = 1
	pcdArgLiveIndex  = 3

	fdArgsPointerMaps   = 0
	fdLocalsPointerMaps = 1
)

// jitPCSPTable encodes the constant spdelta the trampoline's fixed-size frame
// produces. pcvalue seeds val at -1 and zig-zag decodes each uvdelta, so the
// single pair that reaches jitSPDelta is 2*(jitSPDelta+1), varint-encoded.
// The pc-delta must advance past the whole function: pcvalue passes
// `pc == f.entry()` as its "first" flag, and a zero pc-delta would keep that
// true forever, making the zero end-of-table marker read as a real pair and
// driving step off the end of the slice.
func jitPCSPTable(codeLen int) []byte {
	uv := uint32(jitSPDelta) + 1
	uv *= 2
	var tab []byte
	for uv >= 0x80 {
		tab = append(tab, byte(uv)|0x80)
		uv >>= 7
	}
	tab = append(tab, byte(uv))

	pd := uint32((codeLen + pcQuantum - 1) / pcQuantum)
	for pd >= 0x80 {
		tab = append(tab, byte(pd)|0x80)
		pd >>= 7
	}
	tab = append(tab, byte(pd), 0) // pc-delta final byte, then end-of-table 0
	return tab
}

// buildJITModule constructs a moduledata describing one generated trampoline.
// code is the trampoline's machine code (text = &code[0]); the stack argument
// area is argWords argument words followed by retWords result words, with
// argPtrs/retPtrs marking which words hold pointers.
func buildJITModule(code []byte, argWords, retWords int, argPtrs, retPtrs uint64) *jitModuledata {
	text := uintptr(unsafe.Pointer(&code[0]))
	etext := text + uintptr(len(code))

	// funcnametab: offset 0 is the empty name, the function name starts at 1.
	name := "weave.jitstub"
	funcnametab := append([]byte{0}, name...)
	funcnametab = append(funcnametab, 0)

	// _func, followed by pcdata[npcdata]uint32 then funcdata[nfuncdata]uint32.
	const npcdata = 4
	const nfuncdata = 2
	totalWords := argWords + retWords
	fn := rfunc{
		entryOff:  0,
		nameOff:   1,
		args:      int32(totalWords * 8),
		pcsp:      1, // pcsp table at pctab[1]; 0 would read as an external func
		npcdata:   npcdata,
		startLine: 1,
		funcID:    0,
		flag:      0,
		nfuncdata: nfuncdata,
	}
	fnBytes := make([]byte, int(unsafe.Sizeof(fn))+npcdata*4+nfuncdata*4)
	*(*rfunc)(unsafe.Pointer(&fnBytes[0])) = fn
	pcdata := fnBytes[unsafe.Sizeof(fn):]
	// pcdata entries are offsets into pctab; 0 means "no table", which makes
	// pcdatavalue return -1, and getStackMap falls back to stack map 0.
	*(*uint32)(unsafe.Pointer(&pcdata[0*4])) = 0 // UnsafePoint
	*(*uint32)(unsafe.Pointer(&pcdata[1*4])) = 0 // StackMapIndex
	*(*uint32)(unsafe.Pointer(&pcdata[2*4])) = 0 // InlTreeIndex
	*(*uint32)(unsafe.Pointer(&pcdata[3*4])) = 0 // ArgLiveIndex

	// funcdata entries are byte offsets into the gofunc data block.
	funcdata := fnBytes[int(unsafe.Sizeof(fn))+npcdata*4:]

	// Stack maps, laid out inline in the gofunc data block. The args map covers
	// the whole area — argument words then result words — bit i set when word i
	// holds a pointer; the locals map is empty (the stub's own frame holds no
	// pointers the collector must see). The bitmap is variable length
	// (ceil(totalWords/8) bytes), so it lives in a buffer sized for it — a bare
	// &stackmap{} would overflow bytedata past its single byte.
	//
	// gofunc is NOT an array of pointers: funcdata returns gofunc+off and the
	// runtime dereferences that address directly as a *stackmap. Storing a
	// pointer to the stackmap instead of the stackmap itself made the runtime
	// read the pointer value as the stackmap header.
	argBits := (totalWords + 7) / 8
	argsMapSize := int(unsafe.Offsetof(stackmap{}.bytedata)) + argBits
	localsOff := int(alignUp(uintptr(argsMapSize), 4))
	gofunc := make([]byte, localsOff+int(unsafe.Sizeof(stackmap{})))

	am := (*stackmap)(unsafe.Pointer(&gofunc[0]))
	am.n = 1
	am.nbit = int32(totalWords)
	for i := 0; i < totalWords; i++ {
		var isPtr bool
		if i < argWords {
			isPtr = argPtrs&(1<<uint(i)) != 0
		} else {
			isPtr = retPtrs&(1<<uint(i-argWords)) != 0
		}
		if isPtr {
			gofunc[int(unsafe.Offsetof(stackmap{}.bytedata))+i/8] |= 1 << uint(i%8)
		}
	}
	lm := (*stackmap)(unsafe.Pointer(&gofunc[localsOff]))
	lm.n = 1
	lm.nbit = 0

	*(*uint32)(unsafe.Pointer(&funcdata[0*4])) = 0                 // ArgsPointerMaps -> gofunc+0
	*(*uint32)(unsafe.Pointer(&funcdata[1*4])) = uint32(localsOff) // LocalsPointerMaps -> gofunc+localsOff

	// pctab holds just the pcsp table; offset 0 is a sentinel so pcsp's own
	// offset is non-zero (pcvalue treats off==0 as "no table").
	pctab := append([]byte{0}, jitPCSPTable(len(code))...)

	// pclntable holds the _func.
	pclntable := fnBytes

	// ftab: one entry plus a sentinel at etext.
	ftab := []functab{
		{entryoff: 0, funcoff: 0},
		{entryoff: uint32(len(code)), funcoff: uint32(len(pclntable))},
	}

	// findfunctab: a single bucket covering the whole (sub-4KB) text range.
	ffb := make([]findfuncbucket, 1)

	hdr := &pcHeader{
		magic:     0xfffffff1,
		minLC:     pcQuantum,
		ptrSize:   8,
		nfunc:     1,
		nfiles:    0,
		textStart: text,
	}

	md := &jitModuledata{
		pcHeader:    hdr,
		funcnametab: funcnametab,
		pctab:       pctab,
		pclntable:   pclntable,
		ftab:        ftab,
		findfunctab: uintptr(unsafe.Pointer(&ffb[0])),
		minpc:       text,
		maxpc:       etext,
		text:        text,
		etext:       etext,
		gofunc:      uintptr(unsafe.Pointer(&gofunc[0])),
	}

	// Root every allocation the moduledata reaches through pointer or slice
	// fields, so the collector keeps them alive for the life of the process.
	// The moduledata itself is rooted by jitTrampoline.
	jitRoots = append(jitRoots, ffb, gofunc,
		hdr, funcnametab, pctab, pclntable, ftab)
	return md
}

// registerModule appends md to the runtime's moduledata list. The list is
// walked by findfunc on every stack scan, so md (and its text) must never be
// freed — callers root both forever.
func registerModule(md *jitModuledata) {
	lastmoduledatap.next = md
	lastmoduledatap = md
}

// jitTrampoline returns a code pointer for the given stack-area shape, generated
// at runtime, or nil if JIT is unavailable on this platform/version. The code
// pointer is a bare trampoline the itab can call directly; the matching
// moduledata argument map makes any pointers in the stack area visible to the
// collector.
//
// Generation happens at proxy construction (a predictable one-off cost per
// shape), and the result is cached so every proxy sharing a shape reuses the
// same code. The call site itself stays a plain indirect itab call.
func jitTrampoline(sh stubShape) unsafe.Pointer {
	jitMu.Lock()
	defer jitMu.Unlock()

	if code, ok := jitBySh[sh]; ok {
		return code
	}

	code := jitStubCode(sh, uintptr(reflect.ValueOf(Dispatch).Pointer()))
	mem, err := jitExecAlloc(code)
	if err != nil {
		return nil
	}
	base := uintptr(unsafe.Pointer(&mem[0]))

	md := buildJITModule(mem, sh.argWords, sh.retWords, sh.argPtrs, sh.retPtrs)
	registerModule(md)
	jitRoots = append(jitRoots, mem, md)

	ptr := unsafe.Pointer(base)
	jitBySh[sh] = ptr
	return ptr
}
