package weave

// Precise trampolines.
//
// The generic trampoline (stubs_gen_*.go) declares the caller's stack argument
// area as one opaque [stackWindow]byte parameter. That is what lets a single
// generated function serve every method of every interface — but the GC
// description of the caller's outgoing area belongs to the callee, so the byte
// window tells the collector "no pointers here" for an area that may well hold
// some. The window cannot be closed from Go: a pointer argument would have to
// be mirrored into scanned memory before the trampoline's first instruction,
// and the trampoline's static signature cannot know where the pointers are.
//
// A precise trampoline solves it by not being generic. Its trailing parameters
// spell the stack area out word by word — unsafe.Pointer where the method puts
// a pointer, uintptr elsewhere — so the compiler emits an argument pointer map
// that is exactly right, and every pointer in the area is visible to the
// collector from function entry onwards. There is no window at all.
//
// Such a trampoline depends on the method's shape, which is only known once the
// interface exists, so it cannot be pre-generated here: the number of shapes is
// unbounded. Instead StubSource writes the source for the shapes a given set of
// interfaces needs, the program compiles it in, and RegisterStub (called from
// the generated init) hands the code pointer to newTrampoline. Methods that
// pass no pointers through the stack area — the overwhelming majority — keep
// using the generic trampoline and need no generated code.

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"unsafe"
)

// stubMaxWords bounds the shape bitmaps to one uint64 each.
const stubMaxWords = 64

// stubShape identifies one precise trampoline. Two methods with the same index
// and the same stack area pointer shape share a trampoline, whatever their
// argument types are.
type stubShape struct {
	index    int    // method index in the interface, hardcoded in the stub
	argWords int    // words of stack-assigned arguments, up to retOffset
	retWords int    // words of stack-assigned results, from retOffset on
	argPtrs  uint64 // bit i: argument word i holds a pointer
	retPtrs  uint64 // bit i: result word i holds a pointer
}

func (sh stubShape) String() string {
	return fmt.Sprintf("method %d, %d argument words (pointers %#x), %d result words (pointers %#x)",
		sh.index, sh.argWords, sh.argPtrs, sh.retWords, sh.retPtrs)
}

// name is the generated function name for this shape, unique by construction.
func (sh stubShape) name() string {
	return fmt.Sprintf("weaveStub_m%d_a%d_%x_r%d_%x",
		sh.index, sh.argWords, sh.argPtrs, sh.retWords, sh.retPtrs)
}

// words is the number of words of the caller's stack argument area, arguments
// and results together. A precise trampoline declares all of them as its own
// parameters, never as results: a declared result belongs to the compiler,
// which zeroes it on entry and may write it back from a register on return,
// overwriting what the dispatcher stored through the area pointer. Parameters
// are only ever read, so the dispatcher's stores survive — and the pointer map
// covers the result half all the same, which is the entire point.
func (sh stubShape) words() int { return sh.argWords + sh.retWords }

// wordPtrs is the pointer map of the whole area, results shifted into place
// behind the arguments.
func (sh stubShape) wordPtrs() uint64 { return sh.argPtrs | sh.retPtrs<<uint(sh.argWords) }

var (
	stubMu    sync.RWMutex
	stubBySh  = map[stubShape]unsafe.Pointer{}
	uintptrTy = reflect.TypeOf(uintptr(0))
	float64Ty = reflect.TypeOf(float64(0))
	ptrTy     = reflect.TypeOf(unsafe.Pointer(nil))
)

// StubSpec describes one precise trampoline to RegisterStub. Generated code
// fills it in; there is no reason to write one by hand.
type StubSpec struct {
	// Index is the method index the trampoline dispatches to.
	Index int
	// ArgWords and RetWords are the number of pointer-sized words of
	// stack-assigned arguments and of stack-assigned results.
	ArgWords, RetWords int
	// ArgPtrs and RetPtrs mark which of those words hold pointers.
	ArgPtrs, RetPtrs uint64
	// Func is the generated trampoline function itself.
	Func any
}

func (s StubSpec) shape() stubShape {
	return stubShape{
		index:    s.Index,
		argWords: s.ArgWords,
		retWords: s.RetWords,
		argPtrs:  s.ArgPtrs,
		retPtrs:  s.RetPtrs,
	}
}

// RegisterStub makes a generated precise trampoline available to proxy
// construction. It is called from the init function of the file StubSource
// produced, and panics if the function's signature does not match the shape it
// claims — a mismatch would hand the collector a wrong pointer map.
func RegisterStub(s StubSpec) {
	sh := s.shape()
	if sh.words() > stubMaxWords {
		panic("weave.RegisterStub: shape wider than " + fmt.Sprint(stubMaxWords) + " words")
	}
	code := checkStubFunc(sh, s.Func)

	stubMu.Lock()
	defer stubMu.Unlock()
	if old, ok := stubBySh[sh]; ok && old != code {
		panic("weave.RegisterStub: two different trampolines registered for " + sh.String())
	}
	stubBySh[sh] = code
}

// lookupStub returns the registered trampoline for sh, or nil.
func lookupStub(sh stubShape) unsafe.Pointer {
	stubMu.RLock()
	defer stubMu.RUnlock()
	return stubBySh[sh]
}

// wordType is the type the shape requires for word i of a region: a pointer
// word must be declared unsafe.Pointer so the compiler records it in the
// argument pointer map, a non-pointer word must be declared uintptr so the
// collector never scans an integer as a pointer.
func wordType(ptrs uint64, i int) reflect.Type {
	if ptrs&(1<<uint(i)) != 0 {
		return ptrTy
	}
	return uintptrTy
}

// checkStubFunc verifies that fn has exactly the signature shape sh describes
// and returns its code pointer.
func checkStubFunc(sh stubShape, fn any) unsafe.Pointer {
	v := reflect.ValueOf(fn)
	if !v.IsValid() || v.Kind() != reflect.Func {
		panic("weave.RegisterStub: Func is not a function")
	}
	t := v.Type()

	wantIn := intArgRegs + floatArgRegs + sh.words()
	wantOut := intArgRegs + floatArgRegs
	if t.NumIn() != wantIn || t.NumOut() != wantOut {
		panic(fmt.Sprintf("weave.RegisterStub: %s takes %d and returns %d values, want %d and %d for %s"+
			" (was it generated for a different GOARCH?)",
			t, t.NumIn(), t.NumOut(), wantIn, wantOut, sh))
	}
	check := func(what string, i int, got, want reflect.Type) {
		if got != want {
			panic(fmt.Sprintf("weave.RegisterStub: %s %d of %s is %s, want %s (%s)",
				what, i, t, got, want, sh))
		}
	}
	for i := 0; i < intArgRegs; i++ {
		check("parameter", i, t.In(i), uintptrTy)
		check("result", i, t.Out(i), ptrTy)
	}
	for i := 0; i < floatArgRegs; i++ {
		check("parameter", intArgRegs+i, t.In(intArgRegs+i), float64Ty)
		check("result", intArgRegs+i, t.Out(intArgRegs+i), float64Ty)
	}
	for i := 0; i < sh.words(); i++ {
		check("parameter", intArgRegs+floatArgRegs+i, t.In(intArgRegs+floatArgRegs+i), wordType(sh.wordPtrs(), i))
	}
	return unsafe.Pointer(v.Pointer())
}

// --- source generation ------------------------------------------------------

const weaveImportPath = "github.com/jizhuozhi/go-weave"

// StubSource returns the Go source of the precise trampolines the given
// interface types need, as a file belonging to package pkgName. Write it next
// to the code that builds the proxies — a plain `go generate` step — and the
// generated init registers everything at program start:
//
//	//go:generate go run ./internal/gentramp
//
//	func main() {
//		src := weave.StubSource("myapp", reflect.TypeOf((*myapp.Store)(nil)).Elem())
//		os.WriteFile("weave_stubs_gen.go", []byte(src), 0o644)
//	}
//
// Interfaces whose methods pass no pointers through the stack argument area
// need nothing generated; for them the returned file only holds a comment.
//
// The output is specific to the GOARCH it was generated on — register counts
// decide which arguments spill — and carries a matching build constraint, so a
// cross-architecture build needs one file per architecture.
func StubSource(pkgName string, ifaces ...reflect.Type) string {
	return stubSource(pkgName, weaveImportPath, "weave", ifaces...)
}

// stubSource is StubSource with an explicit qualifier for the weave package;
// the tests use an empty one to generate in-package sources.
func stubSource(pkgName, importPath, qual string, ifaces ...reflect.Type) string {
	type use struct {
		shape stubShape
		users []string
	}
	var order []*use
	index := map[stubShape]*use{}

	for _, it := range ifaces {
		if it == nil || it.Kind() != reflect.Interface {
			panic("weave.StubSource: not an interface type")
		}
		for i := 0; i < it.NumMethod(); i++ {
			mt := it.Method(i)
			l := newABILayout(mt.Type)
			if !l.stackPointers() {
				continue
			}
			if l.stackBytes > stackWindow {
				panic(fmt.Sprintf("weave.StubSource: %s.%s needs %d bytes of stack argument area, more than the window of %d",
					it.String(), mt.Name, l.stackBytes, stackWindow))
			}
			sh := l.shape(i)
			u := index[sh]
			if u == nil {
				u = &use{shape: sh}
				index[sh] = u
				order = append(order, u)
			}
			u.users = append(u.users, it.String()+"."+mt.Name+" "+mt.Type.String())
		}
	}

	sel := func(name string) string {
		if qual == "" {
			return name
		}
		return qual + "." + name
	}

	var b strings.Builder
	b.WriteString("// Code generated by weave.StubSource; DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "//go:build %s\n\n", runtime.GOARCH)
	fmt.Fprintf(&b, "package %s\n\n", pkgName)

	if len(order) == 0 {
		b.WriteString("// No precise trampoline is needed: no method passes pointers through the\n" +
			"// caller's stack argument area, so the generic trampoline describes every\n" +
			"// call correctly.\n")
		return b.String()
	}

	b.WriteString("import (\n\t\"runtime\"\n\t\"unsafe\"\n")
	if qual != "" {
		fmt.Fprintf(&b, "\n\t%s %q\n", qual, importPath)
	}
	b.WriteString(")\n\n")

	for _, u := range order {
		writeStub(&b, u.shape, u.users, sel)
	}

	b.WriteString("func init() {\n")
	for _, u := range order {
		sh := u.shape
		fmt.Fprintf(&b, "\t%s(%s{Index: %d, ArgWords: %d, RetWords: %d, ArgPtrs: %#x, RetPtrs: %#x, Func: %s})\n",
			sel("RegisterStub"), sel("StubSpec"), sh.index, sh.argWords, sh.retWords, sh.argPtrs, sh.retPtrs, sh.name())
	}
	b.WriteString("}\n")
	return b.String()
}

// writeStub emits one precise trampoline.
//
// The leading parameters are the architecture's full register file, exactly as
// in the generic trampoline: they consume every argument register, which is
// what forces the trailing words onto the stack, at the very offsets the method
// itself uses. Those trailing words spell out the whole stack argument area —
// the method's stack-assigned arguments and, behind them, the space its
// stack-assigned results will occupy — so the compiler's argument pointer map
// describes every pointer that crosses the area in either direction. Their
// address is the base of the area, which is all the dispatcher needs.
func writeStub(b *strings.Builder, sh stubShape, users []string, sel func(string) string) {
	b.WriteString("// " + sh.name() + " is the precise trampoline for:\n//\n")
	for _, u := range users {
		b.WriteString("//\t" + u + "\n")
	}
	b.WriteString("//\n// " + sh.String() + ".\n")
	b.WriteString("//\n" +
		"// go:nosplit keeps the morestack trampoline from spilling the register\n" +
		"// arguments into home slots the interface call site never reserved, and\n" +
		"// go:norace keeps the race prologue from doing the same.\n")
	b.WriteString("//\n//go:nosplit\n//go:norace\n")

	ptrs := sh.wordPtrs()
	fmt.Fprintf(b, "func %s(\n", sh.name())
	for i := 0; i < intArgRegs; i++ {
		fmt.Fprintf(b, "\ta%d uintptr,\n", i)
	}
	for i := 0; i < floatArgRegs; i++ {
		fmt.Fprintf(b, "\tf%d float64,\n", i)
	}
	for i := 0; i < sh.words(); i++ {
		if i == 0 && sh.argWords > 0 {
			fmt.Fprintf(b, "\t// %d words of stack-assigned arguments:\n", sh.argWords)
		}
		if i == sh.argWords {
			fmt.Fprintf(b, "\t// %d words the stack-assigned results will occupy:\n", sh.retWords)
		}
		fmt.Fprintf(b, "\tw%d %s,\n", i, typeName(ptrs, i))
	}
	b.WriteString(") (\n")
	for i := 0; i < intArgRegs; i++ {
		fmt.Fprintf(b, "\tr%d unsafe.Pointer,\n", i)
	}
	for i := 0; i < floatArgRegs; i++ {
		fmt.Fprintf(b, "\tg%d float64,\n", i)
	}
	b.WriteString(") {\n\tbase := unsafe.Pointer(&w0)\n")

	if sh.retPtrs != 0 {
		b.WriteString("\n" +
			"\t// The result words still hold whatever the caller's frame held\n" +
			"\t// before this call, and the pointer map above now says some of them\n" +
			"\t// are pointers. Clear those before the first safe point — a nosplit\n" +
			"\t// function has none until its first call — so the collector never\n" +
			"\t// sees a stale word through the new map.\n")
		for i := 0; i < sh.retWords; i++ {
			if sh.retPtrs&(1<<uint(i)) != 0 {
				fmt.Fprintf(b, "\t*(*uintptr)(unsafe.Add(base, %d)) = 0\n", (sh.argWords+i)*int(ptrSize))
			}
		}
	}

	// Assign the dispatcher's return values to the named results, then keep the
	// pointer words alive across the call. The whole point of the precise
	// trampoline is its argument pointer map — but a map marks a parameter as a
	// live pointer only where the parameter is actually live, and the body
	// otherwise uses nothing but &w0, the area's address. KeepAlive forces the
	// pointer words live across the Dispatch call, which is what makes the
	// collector scan them in the caller's frame.
	fmt.Fprintf(b, "\n\t")
	for i := 0; i < intArgRegs; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "r%d", i)
	}
	for i := 0; i < floatArgRegs; i++ {
		b.WriteString(", ")
		fmt.Fprintf(b, "g%d", i)
	}
	fmt.Fprintf(b, " = %s(%d", sel("Dispatch"), sh.index)
	for i := 0; i < intArgRegs; i++ {
		fmt.Fprintf(b, ", a%d", i)
	}
	for i := 0; i < floatArgRegs; i++ {
		fmt.Fprintf(b, ", f%d", i)
	}
	b.WriteString(", base)\n")
	for i := 0; i < sh.words(); i++ {
		if ptrs&(1<<uint(i)) != 0 {
			fmt.Fprintf(b, "\truntime.KeepAlive(w%d)\n", i)
		}
	}
	b.WriteString("\treturn\n}\n\n")
}

func typeName(ptrs uint64, i int) string {
	if ptrs&(1<<uint(i)) != 0 {
		return "unsafe.Pointer"
	}
	return "uintptr"
}
