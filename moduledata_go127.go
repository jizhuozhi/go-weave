//go:build go1.27

package weave

// jitModuledata mirrors runtime.moduledata as of Go 1.27+: typedesclen inserted
// between types/etypes, itaboffset/itabsize added after etypes, typelinks/
// itablinks removed, typemap keyed by *_type (an 8-byte map pointer either way).
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

	types, typedesclen, etypes uintptr
	itaboffset, itabsize       uintptr

	rodata   uintptr
	gofunc   uintptr
	epclntab uintptr

	textsectmap []textsect

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
	mdGofuncOff = 344
	mdNextOff   = 560
)
