//go:build go1.23 && !go1.26

package weave

// jitModuledata mirrors runtime.moduledata as of Go 1.23/1.24/1.25: covctrs and
// inittasks present, bad moved next to hasmain.
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

// Expected offsets, verified by TestJITFindfunc.
const (
	mdTextOff   = 176
	mdPctabOff  = 80
	mdGofuncOff = 320
	mdNextOff   = 576
)
