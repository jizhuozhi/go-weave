package weave

// The fast trampoline, shared between the two architectures that ship it. The
// register assignment (abi.go) and this file are architecture independent;
// only the fixed signature of the stub and its dispatch entry differ, and those
// live in fast_arm64.go / fast_amd64.go.

import (
	"reflect"
	"sync"
	"unsafe"
)

// callState is the per-call scratch state, pooled: the Invocation and the
// register spill area it points at. Keeping the register area off the
// goroutine stack also makes it immune to stack moves: a stack growth inside
// an interceptor would leave a stack-allocated regBuf dangling.
type callState struct {
	inv   Invocation
	regs  regBuf
	saved regBuf // argument register snapshot for the fast path of callTarget
}

var statePool = sync.Pool{New: func() any { return new(callState) }}

// fitsFastPath reports whether the method can be expressed by the fixed
// register signature of the generated trampolines: every argument and every
// result must fit in the architecture's register file, with nothing spilling
// to the caller's stack argument area. Methods that do not fit are rejected at
// proxy construction time.
func (m *Method) fitsFastPath() bool {
	l := m.layout
	return l.stackCallArgsSize == 0 && l.ret.stackBytes == l.retOffset
}

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

// stepAddr resolves the memory a step reads from or writes to. Every step of a
// fast path call is register resident (fitsFastPath rejects the rest), so a
// stack step here is unreachable.
func stepAddr(st step, regs *regBuf) unsafe.Pointer {
	switch st.kind {
	case stepIntReg, stepPointer:
		return unsafe.Pointer(&regs.ints[st.ireg])
	case stepFloatReg:
		return unsafe.Pointer(&regs.floats[st.freg])
	}
	panic("weave: bad abi step")
}

// materializeArgs turns the raw register spill area into []reflect.Value.
//
// Arguments that live in exactly one register are described in place, with no
// copy at all. Arguments spread over several registers are gathered into a
// temporary []byte: the collector ignores that buffer, which is safe because
// every byte in it is a copy of data that is still live in the argument
// register snapshot, whose ptrs mirror the collector does scan.
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
		case len(steps) == 1:
			out[i] = reflect.NewAt(t, stepAddr(steps[0], regs)).Elem()
		default:
			if buf == nil {
				buf = make([]byte, l.gatherBytes)
			}
			dst := unsafe.Pointer(&buf[l.argGather[i]])
			for _, st := range steps {
				memcpy(add(dst, st.offset), stepAddr(st, regs), st.size)
			}
			out[i] = reflect.NewAt(t, dst).Elem()
		}
	}
	return out
}

// storeResults writes the results back into the register spill area.
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
		scatterValue(v, t, m.layout.ret.stepsForValue(i), c.regs)
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

// scatterValue copies one result out of v into the registers the ABI assigned
// to it. Fast path results are register resident (fitsFastPath guarantees it).
func scatterValue(v reflect.Value, t reflect.Type, steps []step, regs *regBuf) {
	if len(steps) == 1 {
		st := steps[0]
		dst := stepAddr(st, regs)
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
		}
	}

	// Multi register results that can be taken apart without allocating.
	if len(steps) >= 2 {
		switch t.Kind() {
		case reflect.String:
			s := v.String()
			h := (*[2]uintptr)(unsafe.Pointer(&s))
			writeWord(stepAddr(steps[0], regs), h[0], steps[0].size)
			writeWord(stepAddr(steps[1], regs), h[1], steps[1].size)
			return
		case reflect.Slice:
			h := [3]uintptr{v.Pointer(), uintptr(v.Len()), uintptr(v.Cap())}
			for i := range steps {
				writeWord(stepAddr(steps[i], regs), h[i], steps[i].size)
			}
			return
		case reflect.Complex64, reflect.Complex128:
			x := v.Complex()
			p := (*[2]float64)(unsafe.Pointer(&x))
			*(*float64)(stepAddr(steps[0], regs)) = p[0]
			*(*float64)(stepAddr(steps[1], regs)) = p[1]
			return
		}
	}

	// Everything else: one contiguous copy, sliced up.
	src := v
	if !src.CanAddr() {
		tmp := reflect.New(t).Elem()
		tmp.Set(v)
		src = tmp
	}
	base := src.Addr().UnsafePointer()
	for _, st := range steps {
		memcpy(stepAddr(st, regs), add(base, st.offset), st.size)
	}
}
