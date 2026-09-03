package weave

// Argument materialisation and result scattering, shared between the two
// architectures that ship the trampoline. The register assignment (abi.go) and
// this file are architecture independent; only the fixed signature of the
// dispatch entry differs, and that lives in dispatch_arm64.go /
// dispatch_amd64.go.

import (
	"reflect"
	"sync"
	"unsafe"
)

// callState is the per-call scratch state, pooled: the Invocation, the
// register spill area it points at, and the copy of the method's stack
// argument area. Keeping all of it off the goroutine stack also makes it
// immune to stack moves: a stack growth inside an interceptor would leave
// stack-allocated state dangling.
type callState struct {
	inv      Invocation
	regs     regBuf
	saved    regBuf            // argument register snapshot for the fast path of callTarget
	stackBuf [stackWindow]byte // copy of the caller's stack argument area
}

var statePool = sync.Pool{New: func() any { return new(callState) }}

// regBuf holds the raw argument and result registers for the duration of a call.
//
// It mirrors runtime.RegArgs: ints holds the bit pattern of every integer
// register but is typed as uintptr so the collector never scans it, while ptrs
// is a GC-visible mirror that the dispatcher fills only with the registers that
// actually hold pointers (using the method's layout). Without that split, the
// collector would either miss pointer arguments or, worse, treat a plain
// integer whose bits happen to look like a heap address as a pointer and abort
// with "found bad pointer in Go heap".
type regBuf struct {
	ints   [intArgRegs]uintptr
	ptrs   [intArgRegs]unsafe.Pointer
	floats [floatArgRegs]float64
}

func add(p unsafe.Pointer, off uintptr) unsafe.Pointer {
	return unsafe.Pointer(uintptr(p) + off)
}

func memcpy(dst, src unsafe.Pointer, n uintptr) {
	copy(unsafe.Slice((*byte)(dst), n), unsafe.Slice((*byte)(src), n))
}

// stepAddr resolves the memory a step reads from or writes to. Stack steps
// are relative to the base of the stack argument area, which dispatch points
// at the pooled copy; register steps at the register spill area.
func stepAddr(st step, regs *regBuf, stack unsafe.Pointer) unsafe.Pointer {
	switch st.kind {
	case stepStack:
		return add(stack, st.stkOff)
	case stepIntReg, stepPointer:
		return unsafe.Pointer(&regs.ints[st.ireg])
	case stepFloatReg:
		return unsafe.Pointer(&regs.floats[st.freg])
	}
	panic("weave: bad abi step")
}

// materializeArgs turns the raw register spill area and stack argument area
// copy into []reflect.Value.
//
// Arguments that live in exactly one register or one stack slot are described
// in place, with no copy at all. Arguments spread over several registers are
// gathered into a temporary []byte: the collector ignores that buffer, which
// is safe because every byte in it is a copy of data that is still live in
// the argument register snapshot, whose ptrs mirror the collector does scan.
func (c *Invocation) materializeArgs() []reflect.Value {
	m := c.Method
	l := m.layout
	ft := m.Type
	n := ft.NumIn()
	regs := c.argRegs()

	out := make([]reflect.Value, n)
	var buf []byte
	for i := 0; i < n; i++ {
		t := ft.In(i)
		steps := l.call.stepsForValue(i + 1) // +1 skips the receiver
		switch {
		case t.Size() == 0:
			out[i] = reflect.Zero(t)
		case t.Kind() == reflect.Interface:
			out[i] = materializeIface(t, steps, regs, c.stack)
		case len(steps) == 1:
			out[i] = reflect.NewAt(t, stepAddr(steps[0], regs, c.stack)).Elem()
		default:
			if buf == nil {
				buf = make([]byte, l.gatherBytes)
			}
			dst := unsafe.Pointer(&buf[l.argGather[i]])
			for _, st := range steps {
				memcpy(add(dst, st.offset), stepAddr(st, regs, c.stack), st.size)
			}
			out[i] = reflect.NewAt(t, dst).Elem()
		}
	}
	return out
}

// storeResults writes the results back into the register spill area and, for
// stack-assigned results, the stack area copy.
func (c *Invocation) storeResults(rets []reflect.Value) {
	if c.direct && rets == nil {
		// Register fast path: the target's results are already in the result
		// registers; an interceptor returning nil passes them through.
		return
	}
	m := c.Method
	n := m.Type.NumOut()
	for i := 0; i < n; i++ {
		t := m.Type.Out(i)
		v := m.zeros[i]
		if i < len(rets) && rets[i].IsValid() && rets[i].Type().AssignableTo(t) {
			v = rets[i]
		}
		scatterValue(v, t, m.layout.ret.stepsForValue(i), c.regs, c.stack)
	}
}

// writeWord stores the low size bytes of x at dst. All supported targets are
// little endian, which is where Go puts sub-word values inside a register.
func writeWord(dst unsafe.Pointer, x uintptr, size uintptr) {
	switch size {
	case 8:
		*(*uint64)(dst) = uint64(x)
	case 4:
		*(*uint32)(dst) = uint32(x)
	case 2:
		*(*uint16)(dst) = uint16(x)
	case 1:
		*(*uint8)(dst) = uint8(x)
	default:
		panic("weave: unsupported word size")
	}
}

// writeScalar stores a single word reflect.Value into a register.
func writeScalar(dst unsafe.Pointer, v reflect.Value, size uintptr) {
	var x uintptr
	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			x = 1
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		x = uintptr(v.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Pointer, reflect.UnsafePointer, reflect.Chan,
		reflect.Map, reflect.Func:
		x = v.Pointer()
	default:
		panic("weave: cannot scatter " + v.Kind().String() + " as a scalar")
	}
	writeWord(dst, x, size)
}

// materializeIface reads an interface argument out of the ABI and returns an
// interface-typed reflect.Value for it. An interface travels as two words whose
// first is an *itab — the register ABI's representation — whereas reflect's own
// representation of a value starts with an *abiType, so the two cannot be read
// back with NewAt the way every other type is.
func materializeIface(t reflect.Type, steps []step, regs *regBuf, stack unsafe.Pointer) reflect.Value {
	tab := *(*unsafe.Pointer)(stepAddr(steps[0], regs, stack))
	if tab == nil {
		return reflect.Zero(t)
	}
	var data unsafe.Pointer
	if len(steps) == 1 {
		// A stack-assigned interface is a single step covering the whole
		// 16-byte value: (itab, data) laid out contiguously.
		data = *(*unsafe.Pointer)(add(stepAddr(steps[0], regs, stack), ptrSize))
	} else {
		// Two register steps: itab then data.
		data = *(*unsafe.Pointer)(stepAddr(steps[1], regs, stack))
	}
	// Reassemble the concrete value as an any and let reflect.ValueOf decode
	// it — the one path that treats the (type, data) pair correctly for every
	// kind, pointer included — then box it back into the interface type so the
	// result matches the argument's declared type.
	it := (*itab)(tab)
	var e eface
	e.typ = it.Type
	e.data = data
	slot := reflect.New(t).Elem()
	slot.Set(reflect.ValueOf(*(*any)(unsafe.Pointer(&e))))
	return slot
}

// scatterValue copies one result out of v into the registers or stack slots
// the ABI assigned to it.
func scatterValue(v reflect.Value, t reflect.Type, steps []step, regs *regBuf, stack unsafe.Pointer) {
	if t.Kind() == reflect.Interface {
		scatterIface(v, t, steps, regs, stack)
		return
	}
	if len(steps) == 1 {
		st := steps[0]
		dst := stepAddr(st, regs, stack)
		switch st.kind {
		case stepFloatReg:
			if st.size == 8 {
				*(*float64)(dst) = v.Float()
			} else {
				*(*float32)(dst) = float32(v.Float())
			}
			return
		case stepIntReg, stepPointer:
			writeScalar(dst, v, st.size)
			return
		case stepStack:
			memcpy(dst, valueMem(v, t), st.size)
			return
		}
	}

	// Multi register results that can be taken apart without allocating.
	if len(steps) >= 2 && steps[0].kind != stepStack {
		switch t.Kind() {
		case reflect.String:
			s := v.String()
			h := (*[2]uintptr)(unsafe.Pointer(&s))
			writeWord(stepAddr(steps[0], regs, stack), h[0], steps[0].size)
			writeWord(stepAddr(steps[1], regs, stack), h[1], steps[1].size)
			return
		case reflect.Slice:
			h := [3]uintptr{v.Pointer(), uintptr(v.Len()), uintptr(v.Cap())}
			for i := range steps {
				writeWord(stepAddr(steps[i], regs, stack), h[i], steps[i].size)
			}
			return
		case reflect.Complex64, reflect.Complex128:
			x := v.Complex()
			p := (*[2]float64)(unsafe.Pointer(&x))
			*(*float64)(stepAddr(steps[0], regs, stack)) = p[0]
			*(*float64)(stepAddr(steps[1], regs, stack)) = p[1]
			return
		}
	}

	// Everything else: one contiguous copy, sliced up.
	base := valueMem(v, t)
	for _, st := range steps {
		memcpy(stepAddr(st, regs, stack), add(base, st.offset), st.size)
	}
}

// scatterIface writes an interface result back into the ABI. It is the reverse
// of materializeIface: the caller reads an (itab, data) pair, while reflect
// hands the value over as (type, data), so the value is boxed once through the
// interface type to recover the itab before being stored.
func scatterIface(v reflect.Value, t reflect.Type, steps []step, regs *regBuf, stack unsafe.Pointer) {
	slot := reflect.New(t).Elem()
	slot.Set(v)
	i := (*iface)(unsafe.Pointer(slot.UnsafeAddr()))
	if len(steps) == 1 {
		mem := stepAddr(steps[0], regs, stack)
		*(*unsafe.Pointer)(mem) = unsafe.Pointer(i.tab)
		*(*unsafe.Pointer)(add(mem, ptrSize)) = i.data
		return
	}
	*(*uintptr)(stepAddr(steps[0], regs, stack)) = uintptr(unsafe.Pointer(i.tab))
	*(*unsafe.Pointer)(stepAddr(steps[1], regs, stack)) = i.data
}

// valueMem returns the address of v's storage, copying it into a temporary
// when v is not addressable.
func valueMem(v reflect.Value, t reflect.Type) unsafe.Pointer {
	if v.CanAddr() {
		return v.Addr().UnsafePointer()
	}
	tmp := reflect.New(t).Elem()
	tmp.Set(v)
	return tmp.Addr().UnsafePointer()
}
