package weave

import (
	"reflect"
	"runtime"
	"unsafe"
)

// This file mirrors the parts of internal/abi and runtime that the proxy needs
// to touch. Everything here is unexported: the package's public surface is
// only the proxy API. The layouts are pinned to Go 1.24; the tests exercise
// every offset-sensitive path so that a silent breakage is impossible.

// abiType mirrors internal/abi.Type, the header shared by every runtime type
// descriptor. reflect.Type values handed out by the runtime point at exactly
// this structure, so the conversion in typeOf is a no-op.
type abiType struct {
	Size_       uintptr
	PtrBytes    uintptr // number of (prefix) bytes in the type that can contain pointers
	Hash        uint32  // hash of type; avoids computation in hash tables
	TFlag       uint8   // extra type information flags
	Align_      uint8   // alignment of variable with this type
	FieldAlign_ uint8   // alignment of struct field with this type
	Kind_       uint8   // enumeration for C
	Equal       func(unsafe.Pointer, unsafe.Pointer) bool
	GCData      *byte // garbage collection data
	Str         int32 // string form
	PtrToThis   int32 // type for pointer to this type, may be zero
}

// abiName mirrors internal/abi.Name.
type abiName struct {
	Bytes *byte
}

// imethod mirrors internal/abi.Imethod: one method of an interface type.
type imethod struct {
	Name int32 // NameOff
	Typ  int32 // TypeOff
}

// interfaceType mirrors internal/abi.InterfaceType.
type interfaceType struct {
	abiType
	PkgPath abiName
	Methods []imethod // sorted by name
}

// itab mirrors internal/abi.ITab, the dispatch table living in the first word
// of every non-empty interface value.
//
// Fun is declared with length 1 but is really a variable sized array of bare
// code pointers; the memory past this struct is indexed manually.
type itab struct {
	Inter *interfaceType
	Type  *abiType
	Hash  uint32 // copy of Type.Hash, used for type switches
	_     [4]byte
	Fun   [1]uintptr
}

// eface is the layout of an `any`.
type eface struct {
	typ  *abiType
	data unsafe.Pointer
}

// iface is the layout of a non-empty interface value.
type iface struct {
	tab  *itab
	data unsafe.Pointer
}

// efaceData returns the data word of an interface value.
func efaceData(i any) unsafe.Pointer {
	return (*eface)(unsafe.Pointer(&i)).data
}

// typeOf returns the runtime type descriptor behind a reflect.Type.
func typeOf(t reflect.Type) *abiType {
	return (*abiType)(efaceData(t))
}

// interfaceTypeOf converts a reflect.Type of kind Interface into the runtime
// *interfaceType it already is.
func interfaceTypeOf(t reflect.Type) *interfaceType {
	if t.Kind() != reflect.Interface {
		panic("weave: not an interface type: " + t.String())
	}
	return (*interfaceType)(unsafe.Pointer(typeOf(t)))
}

// forgeITab allocates a synthetic itab for (inter, proxyType) whose method
// entries point at the given code pointers.
//
// inter must be the *interfaceType of the proxied interface. funs must be
// ordered exactly like inter.Methods, which is the same order as
// reflect.Type.Method(i) reports.
//
// The itab is stored in a []unsafe.Pointer so that the garbage collector scans
// it and keeps the referenced code alive: unlike the runtime, which allocates
// real itabs with persistentalloc, ours must live on the scanned heap.
func forgeITab(inter *interfaceType, proxyType *abiType, funs []unsafe.Pointer) *itab {
	n := len(inter.Methods)
	if n != len(funs) {
		panic("weave: method count mismatch")
	}
	// 3 leading words: Inter, Type, Hash(+padding). Fun starts at word 3,
	// which is byte offset 24 == unsafe.Offsetof(itab{}.Fun).
	w := make([]unsafe.Pointer, 3+n)
	w[0] = unsafe.Pointer(inter)
	w[1] = unsafe.Pointer(proxyType)
	// Hash shares a word with the 4 padding bytes; only write the low half.
	*(*uint32)(unsafe.Pointer(&w[2])) = proxyType.Hash
	for i, f := range funs {
		w[3+i] = f
	}
	return (*itab)(unsafe.Pointer(&w[0]))
}

// makeIface assembles a non-empty interface value out of a forged itab and a
// data pointer, writing it straight into *dst.
//
// T must be an interface type. Note that this cannot be done through an `any`
// parameter: the first word of an interface value is an *itab while the first
// word of an `any` is an *_type, and Go would convert between the two on the
// way in, destroying the itab.
func makeIface[T any](dst *T, tab *itab, data unsafe.Pointer) {
	i := (*iface)(unsafe.Pointer(dst))
	i.tab = tab
	i.data = data
}

// itabFun returns the code pointer of method k of a runtime itab with n
// methods. Method order matches reflect's (sorted by name), which is also the
// order the itab's Fun array uses.
func itabFun(tab *itab, n, k int) unsafe.Pointer {
	funs := unsafe.Slice(&tab.Fun[0], n)
	return unsafe.Pointer(funs[k])
}

// targetITab converts target to ifaceType through ordinary assignment and
// returns the resulting interface value's runtime itab and data word. ok is
// false when the target's type does not fully implement the interface, in
// which case no register fast path is available and calls fall back to
// reflect.
func targetITab(target any, ifaceType reflect.Type) (tab *itab, data unsafe.Pointer, ok bool) {
	tv := reflect.ValueOf(target)
	if !tv.IsValid() || !tv.Type().Implements(ifaceType) {
		return nil, nil, false
	}
	slot := reflect.New(ifaceType).Elem()
	slot.Set(tv)
	i := (*iface)(unsafe.Pointer(slot.UnsafeAddr()))
	return i.tab, i.data, true
}

// --- layout self-check ------------------------------------------------------
//
// The package forges runtime data structures whose layouts carry no
// compatibility promise. At init, every offset the forgery depends on is
// validated against a real, runtime-built itab, so an unsupported Go version
// fails at startup with a clear panic instead of corrupting memory later.

type itabProbe struct{ marker int }

func (p *itabProbe) Probe() int { return p.marker }

type itabProbeIface interface{ Probe() int }

func init() {
	var v itabProbeIface = &itabProbe{marker: 42}
	real := (*iface)(unsafe.Pointer(&v)).tab

	// itab.Inter must land on a type descriptor of interface kind, which
	// pins both the itab header and the interfaceType embedding.
	if real.Inter == nil || real.Inter.Kind_ != uint8(reflect.Interface) {
		panic("weave: itab.Inter offset mismatch; unsupported Go version, " + runtimeVersion())
	}
	// itab.Hash is specified to be a copy of the concrete type's hash, which
	// pins the Hash fields of both itab and abiType.
	if real.Hash != real.Type.Hash {
		panic("weave: itab.Hash offset mismatch; unsupported Go version, " + runtimeVersion())
	}
	// For a pointer-receiver method, itab.Fun[0] is the method's own entry,
	// which pins the Fun offset (the field the dispatch call jumps through).
	fun := itabFun(real, 1, 0)
	if fun == nil || uintptr(fun) != reflect.ValueOf((*itabProbe).Probe).Pointer() {
		panic("weave: itab.Fun offset mismatch; unsupported Go version, " + runtimeVersion())
	}
}

func runtimeVersion() string { return runtime.Version() }
