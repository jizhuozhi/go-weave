package weave

import (
	"reflect"
	"unsafe"
)

// Method describes one proxied method of one proxy.
type Method struct {
	// Name is the method name as spelled in the interface.
	Name string
	// Type is the method's func type without a receiver.
	Type reflect.Type
	// Index is the method's index in the interface method table, which is
	// also its index into itab.Fun and its trampoline slot.
	Index int

	layout *abiLayout

	// targetFn is the bound method value on the proxied object, used by the
	// reflect fallback path. It is the zero Value for a mock proxy or when
	// the target does not have the method.
	targetFn reflect.Value

	// targetFun is the code pointer of the target's own implementation of
	// this method (its itab.Fun entry), and targetData the target's receiver
	// word. Together they enable the register fast path of callTarget. They
	// are nil unless the target's type fully implements the interface.
	targetFun  unsafe.Pointer
	targetData unsafe.Pointer

	// zeros holds the zero value of every result, used when nobody calls
	// Proceed or when the target panics and the panic is swallowed.
	zeros []reflect.Value

	// codePtr is the code pointer stored in itab.Fun[Index].
	codePtr unsafe.Pointer
}

// IsVariadic reports whether the method is variadic.
func (m *Method) IsVariadic() bool { return m.Type.IsVariadic() }

// NumIn is the number of arguments, receiver excluded.
func (m *Method) NumIn() int { return m.Type.NumIn() }

// NumOut is the number of results.
func (m *Method) NumOut() int { return m.Type.NumOut() }

// String returns "Name(ArgTypes) ResultTypes".
func (m *Method) String() string { return m.Name + " " + m.Type.String() }

// Interceptor is one link of the around-advice chain. It must eventually call
// Proceed unless it intends to short circuit the call, in which case it returns
// the results itself.
//
// Returning nil is allowed when the method has no results.
type Interceptor func(ctx *Invocation) []reflect.Value

// Invocation is the context of a single proxied call. It is not safe to use
// after the call returns.
type Invocation struct {
	// Proxy is the proxy the call arrived on.
	Proxy *Proxy
	// Method is the method being invoked.
	Method *Method

	chain []Interceptor
	next  int
	args  []reflect.Value

	// regs is the register spill area the trampoline forwarded to dispatch.
	// It stays valid (and visible to the collector) for the whole call, so
	// arguments can be read out of it lazily.
	regs *regBuf

	// stack is the base of the stack argument area copy for methods whose
	// arguments or results spill past the register file; nil otherwise.
	stack unsafe.Pointer

	// snap is a snapshot of the argument registers taken before the register
	// fast path of callTarget reissues the call: the call's results land in
	// the same registers, destroying the arguments.
	snap    *regBuf
	hasSnap bool
	// direct marks a callTarget that delivered its results straight into the
	// result registers, so storeResults must not scatter over them.
	direct bool
}

// Arg returns the i'th argument.
//
// This materialises every argument on first use; interceptors that never look
// at the arguments pay nothing.
func (c *Invocation) Arg(i int) reflect.Value { return c.Args()[i] }

// NumArg is the number of arguments.
func (c *Invocation) NumArg() int { return c.Method.Type.NumIn() }

// argRegs returns the register area still holding the original arguments:
// the snapshot once the live registers have been overwritten by results.
func (c *Invocation) argRegs() *regBuf {
	if c.hasSnap {
		return c.snap
	}
	return c.regs
}

// Args returns the arguments as a slice. The slice may be modified in place to
// rewrite the call; the new values are used when Proceed is finally reached.
func (c *Invocation) Args() []reflect.Value {
	if c.args == nil {
		if c.regs != nil {
			c.args = c.materializeArgs()
		} else {
			c.args = []reflect.Value{}
		}
	}
	return c.args
}

// SetArg rewrites the i'th argument.
func (c *Invocation) SetArg(i int, v reflect.Value) { c.Args()[i] = v }

// Proceed runs the rest of the interceptor chain and finally the real method.
//
// Proceed may be called more than once; every call re-executes the remaining
// chain and the target.
func (c *Invocation) Proceed() []reflect.Value {
	if c.next < len(c.chain) {
		h := c.chain[c.next]
		c.next++
		return h(c)
	}
	return c.callTarget()
}

// callTarget invokes the real method, or returns zero values for a mock.
//
// When no interceptor has materialised the arguments (c.args == nil) and the
// target statically implements the interface, it takes the register fast
// path: the original argument registers are replayed straight at the target's
// own method code with the receiver rebound, bypassing reflect entirely. The
// results land directly in the result registers; the nil return together with
// c.direct tells storeResults to leave them there.
func (c *Invocation) callTarget() []reflect.Value {
	m := c.Method
	if m.targetFun == nil {
		if !m.targetFn.IsValid() {
			return m.zeros
		}
		// The target does not fully implement the interface; this method
		// still has a bound method value to delegate through.
		if m.IsVariadic() {
			return m.targetFn.CallSlice(c.Args())
		}
		return m.targetFn.Call(c.Args())
	}
	// Register fast path: only for methods that pass nothing on the stack —
	// redial replays registers alone.
	if c.args == nil && m.layout.stackBytes == 0 {
		if c.hasSnap {
			// A previous Proceed already overwrote the argument registers
			// with results; replay from the snapshot.
			c.regs.ints = c.snap.ints
			c.regs.floats = c.snap.floats
		} else {
			// The call below destroys the arguments in the live registers;
			// snapshot them first, ptrs mirror included, so that a later
			// Args() or a second Proceed still sees them and the collector
			// keeps every pointer argument alive.
			*c.snap = *c.regs
			c.hasSnap = true
		}
		// Rebind the receiver from the proxy to the target and reissue the
		// call. Bit 0 of ptrMask is the receiver, which is always a pointer.
		c.regs.ints[0] = uintptr(m.targetData)
		c.regs.ptrs[0] = m.targetData
		redial(m.targetFun, c.regs)
		c.direct = true
		return nil
	}
	// Reflect fallback: the arguments were materialised (an interceptor
	// touched them) or the method passes arguments or results on the stack,
	// which redial cannot replay.
	if m.IsVariadic() {
		return m.targetFn.CallSlice(c.Args())
	}
	return m.targetFn.Call(c.Args())
}
