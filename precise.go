package weave

// Precise trampolines, generated at runtime.
//
// The generic trampoline (stubs_gen_*.go) declares the caller's stack argument
// area as one opaque [stackWindow]byte parameter. That is what lets a single
// generated function serve every method of every interface — but the GC
// description of the caller's outgoing area belongs to the callee, so the byte
// window tells the collector "no pointers here" for an area that may well hold
// some.
//
// A method that moves pointers through the stack argument area needs a
// trampoline whose argument pointer map describes that area word by word — a
// "precise" trampoline. It is compiled at proxy construction, not ahead of time:
// jitTrampoline mmaps an executable page, writes the trampoline's machine code
// (the same register shuffle the generic stubs perform), and builds a moduledata
// whose argument map marks exactly the pointer words. The cost is a one-off per
// shape, cached; the call site stays a plain indirect itab call.

import "fmt"

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

// words is the number of words of the caller's stack argument area, arguments
// and results together.
func (sh stubShape) words() int { return sh.argWords + sh.retWords }
