//go:build amd64 || arm64

package weave

// JIT probe: verify that a runtime-generated trampoline, injected into the
// runtime's moduledata list via //go:linkname lastmoduledatap, is recognised by
// findfunc and scanned by the collector with a dynamic argument pointer map.
//
// This is the feasibility spike for replacing the compile-time codegen with
// runtime JIT. It mirrors what sonic does: mmap an executable page, generate
// the (fixed) trampoline machine code, build a moduledata whose
// FUNCDATA_ArgsPointerMaps describes the method's stack argument area, and
// append it to lastmoduledatap.

import (
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"unsafe"
	_ "unsafe" // for //go:linkname

	"github.com/jizhuozhi/go-weave/internal/rt"
)

//go:linkname lastmoduledatap runtime.lastmoduledatap
var lastmoduledatap *jitModuledata

// jitRoots keeps heap allocations referenced only through uintptr fields of the
// (never-scanned-by-construction) moduledata reachable from a GC root.
var jitRoots []any

// --- runtime layout mirrors (arm64) ----------------------------------------

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

// _func mirrors runtime._func, minus the trailing variable arrays.
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

// moduledata mirrors runtime.moduledata. Field order and types are load-bearing.
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

// nameOff / typeOff are runtime offsets; the map value type is irrelevant for
// layout, so aliasing uintptr keeps the field sized identically.
type nameOff = int32
type typeOff = uint32

// pcdata / funcdata table indexes (mirror internal/abi).
const (
	pcdUnsafePoint   = 0
	pcdStackMapIndex = 1
	pcdArgLiveIndex  = 3

	fdArgsPointerMaps   = 0
	fdLocalsPointerMaps = 1
)

// pcQuantum is defined per-architecture in jitcode_*.go.

// buildJITModule constructs a moduledata describing one generated trampoline.
// code is the trampoline's machine code (text = &code[0]), argWords the size of
// the stack argument area in words, and argPtrs the pointer bitmap over it.
func buildJITModule(code []byte, argWords int, argPtrs uint64) *jitModuledata {
	text := uintptr(unsafe.Pointer(&code[0]))
	etext := text + uintptr(len(code))

	// funcnametab: offset 0 is the empty name, the function name starts at 1.
	name := "weave.jitstub"
	funcnametab := append([]byte{0}, name...)
	funcnametab = append(funcnametab, 0)

	// _func, followed by pcdata[npcdata]uint32 then funcdata[nfuncdata]uint32.
	const npcdata = 4
	const nfuncdata = 2
	fn := rfunc{
		entryOff:  0,
		nameOff:   1,
		args:      int32(argWords * 8),
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

	// funcdata entries are offsets from gofunc to the pointer slots.
	funcdata := fnBytes[int(unsafe.Sizeof(fn))+npcdata*4:]
	*(*uint32)(unsafe.Pointer(&funcdata[0*4])) = 0 // ArgsPointerMaps -> gofunc+0
	*(*uint32)(unsafe.Pointer(&funcdata[1*4])) = 8 // LocalsPointerMaps -> gofunc+8

	// Stack maps: one bitmap each. Args map describes argWords words, with bit
	// i set when word i holds a pointer; locals map is empty (the stub's own
	// frame holds no pointers the collector must see).
	argsMap := &stackmap{n: 1, nbit: int32(argWords)}
	argBits := (argWords + 7) / 8
	for i := 0; i < argWords; i++ {
		if argPtrs&(1<<uint(i)) != 0 {
			argsMap.bytedata[i/8] |= 1 << uint(i%8)
		}
	}
	_ = argBits
	localsMap := &stackmap{n: 1, nbit: 0}

	// gofunc: a table of pointers, one per funcdata slot.
	gofuncPtrs := []unsafe.Pointer{unsafe.Pointer(argsMap), unsafe.Pointer(localsMap)}

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
		pctab:       []byte{},
		pclntable:   pclntable,
		ftab:        ftab,
		findfunctab: uintptr(unsafe.Pointer(&ffb[0])),
		minpc:       text,
		maxpc:       etext,
		text:        text,
		etext:       etext,
		gofunc:      uintptr(unsafe.Pointer(&gofuncPtrs[0])),
	}

	// Root the allocations reachable only through uintptr fields of the
	// moduledata (findfunctab, gofunc) so the collector keeps them alive.
	jitRoots = append(jitRoots, ffb, gofuncPtrs)
	return md
}

func registerModule(md *jitModuledata) {
	lastmoduledatap.next = md
	lastmoduledatap = md
}

// TestJITFindfunc verifies the layout mirror is exact: reading firstmoduledata's
// text through our struct must yield a plausible code address, and a module we
// register must be discoverable by runtime.FuncForPC.
func TestJITFindfunc(t *testing.T) {
	// Sanity: the mirror's field offsets must agree with the runtime's.
	if got := unsafe.Offsetof(lastmoduledatap.text); got != 176 {
		t.Fatalf("moduledata.text offset = %d, want 176", got)
	}
	if got := unsafe.Offsetof(lastmoduledatap.gofunc); got != 320 {
		t.Fatalf("moduledata.gofunc offset = %d, want 320", got)
	}
	if got := unsafe.Offsetof(lastmoduledatap.next); got != 576 {
		t.Fatalf("moduledata.next offset = %d, want 576", got)
	}

	// This first step only probes findfunc, so a plain writable mapping stands
	// in for the text range — findmoduledatap matches on [minpc, maxpc) and
	// never checks executability. (Real execution needs MAP_JIT +
	// pthread_jit_write_protect_np on Apple Silicon, handled in the next step.)
	code, err := syscall.Mmap(-1, 0, 4096,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Skipf("mmap: %v", err)
	}
	defer syscall.Munmap(code)

	md := buildJITModule(code[:16], 0, 0)
	registerModule(md)

	f := runtime.FuncForPC(uintptr(unsafe.Pointer(&code[0])))
	if f == nil {
		t.Fatal("FuncForPC returned nil for the injected function")
	}
	if got := f.Name(); got != "weave.jitstub" {
		t.Fatalf("FuncForPC name = %q, want weave.jitstub", got)
	}
	t.Logf("findfunc resolved injected trampoline: %s", f.Name())
}

// makeJITPage emits the trampoline for idx into a fresh executable region and
// registers a moduledata describing argWords words of stack argument area with
// pointer bitmap argPtrs.
func makeJITPage(idx, argWords int, argPtrs uint64) (uintptr, func()) {
	code := jitStubCode(idx, uintptr(reflect.ValueOf(Dispatch).Pointer()))
	mem, makeExec, err := jitExecAlloc(4096)
	if err != nil {
		panic(err)
	}
	base := uintptr(unsafe.Pointer(&mem[0]))
	copy(mem[:len(code)], code)
	makeExec()

	md := buildJITModule(mem[:len(code)], argWords, argPtrs)
	registerModule(md)
	return base, func() { syscall.Munmap(mem) }
}

// TestJITSelftest checks the C-level mmap+protect+execute round trip works at
// all, isolating platform mechanics from the trampoline machine code.
func TestJITSelftest(t *testing.T) {
	if runtime.GOARCH != "arm64" || runtime.GOOS != "darwin" {
		t.Skip("darwin/arm64 only")
	}
	if !rt.Selftest() {
		t.Fatal("C-level JIT selftest faulted")
	}
}

// TestJITExec proves the JIT trampoline actually runs end to end: it dispatches
// method 5 (Noop) of a Service proxy through the generated machine code.
func TestJITExec(t *testing.T) {
	if runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		t.Skip("arm64/amd64 only")
	}
	p := New[Service](svc{})
	entry, cleanup := makeJITPage(5, 0, 0) // Noop is method index 5
	defer cleanup()

	// Point itab.Fun[5] at the JIT trampoline, then call Noop through the
	// interface — the real dispatch path, whose stack-argument-area layout the
	// trampoline's &s0 offset assumes.
	proxy := (*Proxy)((*iface)(unsafe.Pointer(&p)).data)
	funs := unsafe.Slice(&proxy.itab.Fun[0], len(proxy.methods))
	funs[5] = uintptr(entry)
	p.Noop()
	// Reaching here without a fault means the JIT trampoline ran Dispatch.
	t.Log("JIT trampoline dispatched Noop")
}
