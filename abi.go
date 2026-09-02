package weave

// This file is a port of the register assignment half of reflect/abi.go, which
// is unexported. It answers the question the assembly trampoline needs:
// "argument i of this method lives in which registers, or at which offset of
// the caller's stack argument area?"
//
// The algorithm is Go's documented internal ABI: arguments are considered in
// order, each one is flattened into a sequence of "steps", a step takes either
// an integer register, a float register, or a stack slot. If an argument does
// not fit in the remaining registers it is stack assigned, and everything
// after it stays on the stack.

import (
	"reflect"
	"unsafe"
)

var ptrSize = unsafe.Sizeof(uintptr(0))

func alignUp(x, a uintptr) uintptr { return (x + a - 1) &^ (a - 1) }

type stepKind uint8

const (
	stepStack    stepKind = iota // value lives in the stack argument area
	stepIntReg                   // value lives in an integer argument register
	stepPointer                  // value lives in an integer register and is a pointer
	stepFloatReg                 // value lives in a float argument register
)

// step is one ABI "instruction": move size bytes between offset inside the Go
// value and (ireg | freg | stkOff) inside the call frame.
type step struct {
	kind   stepKind
	offset uintptr // offset of this part inside the Go value
	size   uintptr // size of this part in bytes
	stkOff uintptr // offset inside the stack argument area
	ireg   int     // integer register index
	freg   int     // float register index
}

// seq is the flattened translation plan for a list of Go values (arguments, or
// results).
type seq struct {
	steps      []step
	valueStart []int
	stackBytes uintptr
	iregs      int
	fregs      int
}

// stepsForValue returns the steps making up the i'th value.
func (s *seq) stepsForValue(i int) []step {
	start := s.valueStart[i]
	end := len(s.steps)
	if i < len(s.valueStart)-1 {
		end = s.valueStart[i+1]
	}
	return s.steps[start:end]
}

// addRcvr reserves the receiver slot. Interface calls always pass the receiver
// as a single pointer-shaped word in the first integer register.
func (s *seq) addRcvr() {
	s.valueStart = append(s.valueStart, len(s.steps))
	if !s.assignIntN(0, ptrSize, 1, 0b1) {
		s.stackAssign(ptrSize, ptrSize)
	}
}

// addArg extends the sequence with one Go value. It returns the single step if
// the value ended up on the stack, nil otherwise.
func (s *seq) addArg(t reflect.Type) *step {
	pStart := len(s.steps)
	s.valueStart = append(s.valueStart, pStart)

	if t.Size() == 0 {
		// Zero sized types take no space but still force the next argument to
		// be aligned, which is what stackAssign below would have done.
		s.stackBytes = alignUp(s.stackBytes, uintptr(t.Align()))
		return nil
	}

	old := *s
	if !s.regAssign(t, 0) {
		*s = old
		s.stackAssign(t.Size(), uintptr(t.Align()))
		return &s.steps[len(s.steps)-1]
	}
	return nil
}

// regAssign is the complete register assignment algorithm of the Go ABI,
// transcribed from reflect/abi.go onto reflect.Type.
func (s *seq) regAssign(t reflect.Type, offset uintptr) bool {
	switch t.Kind() {
	case reflect.UnsafePointer, reflect.Pointer, reflect.Chan, reflect.Map, reflect.Func:
		return s.assignIntN(offset, t.Size(), 1, 0b1)
	case reflect.Bool,
		reflect.Int, reflect.Uint,
		reflect.Int8, reflect.Uint8,
		reflect.Int16, reflect.Uint16,
		reflect.Int32, reflect.Uint32,
		reflect.Uintptr:
		return s.assignIntN(offset, t.Size(), 1, 0b0)
	case reflect.Int64, reflect.Uint64:
		if ptrSize == 4 {
			return s.assignIntN(offset, 4, 2, 0b0)
		}
		return s.assignIntN(offset, 8, 1, 0b0)
	case reflect.Float32, reflect.Float64:
		return s.assignFloatN(offset, t.Size(), 1)
	case reflect.Complex64:
		return s.assignFloatN(offset, 4, 2)
	case reflect.Complex128:
		return s.assignFloatN(offset, 8, 2)
	case reflect.String:
		return s.assignIntN(offset, ptrSize, 2, 0b01)
	case reflect.Interface:
		return s.assignIntN(offset, ptrSize, 2, 0b10)
	case reflect.Slice:
		return s.assignIntN(offset, ptrSize, 3, 0b001)
	case reflect.Array:
		switch t.Len() {
		case 0:
			return true
		case 1:
			return s.regAssign(t.Elem(), offset)
		default:
			return false
		}
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !s.regAssign(f.Type, offset+f.Offset) {
				return false
			}
		}
		return true
	}
	panic("weave: unsupported argument kind " + t.Kind().String() + " (" + t.String() + ")")
}

// assignIntN reserves n consecutive integer registers, each size bytes wide.
// Bit i of ptrMap marks the i'th register as holding a pointer.
func (s *seq) assignIntN(offset, size uintptr, n int, ptrMap uint8) bool {
	if n > 8 {
		panic("weave: invalid n")
	}
	if ptrMap != 0 && size != ptrSize {
		panic("weave: pointer map for non pointer sized value")
	}
	if s.iregs+n > intArgRegs {
		return false
	}
	for i := 0; i < n; i++ {
		kind := stepIntReg
		if ptrMap&(uint8(1)<<i) != 0 {
			kind = stepPointer
		}
		s.steps = append(s.steps, step{
			kind:   kind,
			offset: offset + uintptr(i)*size,
			size:   size,
			ireg:   s.iregs,
		})
		s.iregs++
	}
	return true
}

// assignFloatN reserves n consecutive float registers.
func (s *seq) assignFloatN(offset, size uintptr, n int) bool {
	if s.fregs+n > floatArgRegs || floatRegSize < size {
		return false
	}
	for i := 0; i < n; i++ {
		s.steps = append(s.steps, step{
			kind:   stepFloatReg,
			offset: offset + uintptr(i)*size,
			size:   size,
			freg:   s.fregs,
		})
		s.fregs++
	}
	return true
}

// stackAssign places one value in the stack argument area.
func (s *seq) stackAssign(size, alignment uintptr) {
	s.stackBytes = alignUp(s.stackBytes, alignment)
	s.steps = append(s.steps, step{
		kind:   stepStack,
		offset: 0, // stack steps describe whole arguments
		size:   size,
		stkOff: s.stackBytes,
	})
	s.stackBytes += size
}

// abiLayout is the call frame description of one method, receiver included.
type abiLayout struct {
	call seq // receiver followed by the arguments
	ret  seq // results

	// stackCallArgsSize is the number of bytes the caller reserved for stack
	// assigned arguments. retOffset is where stack assigned results start.
	stackCallArgsSize uintptr
	retOffset         uintptr

	// stackBytes is the size of the caller's whole argument area, arguments
	// and results included.
	stackBytes uintptr

	// argGather[i] is where argument i is assembled inside the temporary
	// gather buffer, or ^uintptr(0) when the argument occupies a single
	// register or a single stack slot and needs no assembly at all.
	argGather   []uintptr
	gatherBytes uintptr

	// ptrMask has bit i set when the i'th integer argument register holds a
	// pointer. The dispatcher copies only those registers into a GC-visible
	// mirror, so that the collector never scans a plain integer as a pointer.
	ptrMask uint32
}

// newABILayout computes the frame layout of a method whose reflect func type
// (receiver excluded) is ft.
func newABILayout(ft reflect.Type) *abiLayout {
	if ft.Kind() != reflect.Func {
		panic("weave: not a func type")
	}
	l := &abiLayout{}

	l.call.addRcvr()
	for i := 0; i < ft.NumIn(); i++ {
		l.call.addArg(ft.In(i))
	}

	l.stackCallArgsSize = l.call.stackBytes
	l.retOffset = alignUp(l.stackCallArgsSize, ptrSize)

	// Results never share stack space with arguments.
	l.ret.stackBytes = l.retOffset
	for i := 0; i < ft.NumOut(); i++ {
		l.ret.addArg(ft.Out(i))
	}

	l.stackBytes = alignUp(l.retOffset+l.ret.stackBytes, ptrSize)

	// Which integer argument registers hold pointers.
	for _, st := range l.call.steps {
		if st.kind == stepPointer {
			l.ptrMask |= 1 << uint(st.ireg)
		}
	}

	// Pre-compute the gather buffer layout: arguments spread over several
	// registers (strings, slices, small structs) have to be copied into one
	// contiguous piece of memory before reflect can describe them.
	l.argGather = make([]uintptr, ft.NumIn())
	off := uintptr(0)
	for i := 0; i < ft.NumIn(); i++ {
		steps := l.call.stepsForValue(i + 1) // +1 skips the receiver
		t := ft.In(i)
		if len(steps) > 1 && t.Size() > 0 {
			off = alignUp(off, ptrSize)
			l.argGather[i] = off
			off += t.Size()
		} else {
			l.argGather[i] = ^uintptr(0)
		}
	}
	l.gatherBytes = off
	return l
}

// argStepCount is the number of values described by call, i.e. one receiver
// plus one per argument.
func (l *abiLayout) argStepCount() int { return len(l.call.valueStart) }
