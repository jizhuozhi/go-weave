package weave

import (
	"fmt"
	"reflect"
	"unsafe"
)

// Proxy is a runtime generated implementation of an interface. Build one with
// New or NewOf and convert it to the interface with As; the resulting value is
// a first-class interface value.
type Proxy struct {
	typ     reflect.Type // the proxied interface type
	target  reflect.Value
	chain   []Interceptor
	methods []*Method
	itab    *itab
}

// New returns a proxy for T (which must be an interface type) delegating to
// target and running the interceptors in order. A nil target builds a mock:
// every method returns zero values unless an interceptor says otherwise.
//
// The returned value is a real interface value of type T, assembled from an
// itab built at runtime: it can be stored, passed and called like any other T
// even though no Go type ever declared that it implements T. The one thing it
// cannot do is survive a conversion to `any` followed by a type assertion back
// to T — see the package documentation.
func New[T any](target T, interceptors ...Interceptor) T {
	typ := reflect.TypeOf((*T)(nil)).Elem()
	if typ.Kind() != reflect.Interface {
		panic("weave.New: T must be an interface type, got " + typ.String())
	}
	return As[T](NewOf(typ, target, interceptors...))
}

// NewOf is the reflection based form of New. Call As to turn the result into a
// usable interface value.
func NewOf(ifaceType reflect.Type, target any, interceptors ...Interceptor) *Proxy {
	if ifaceType.Kind() != reflect.Interface {
		panic("weave.NewOf: not an interface type: " + ifaceType.String())
	}
	inter := interfaceTypeOf(ifaceType)

	p := &Proxy{
		typ:   ifaceType,
		chain: append([]Interceptor(nil), interceptors...),
	}
	if target != nil {
		p.target = reflect.ValueOf(target)
	}

	// When the target's type fully implements the interface, extract its
	// runtime itab once: its Fun entries are the code pointers the register
	// fast path replays calls at.
	var tTab *itab
	var tData unsafe.Pointer
	if target != nil {
		tTab, tData, _ = targetITab(target, ifaceType)
	}

	n := ifaceType.NumMethod()
	p.methods = make([]*Method, n)
	fvs := make([]unsafe.Pointer, n)
	for i := 0; i < n; i++ {
		p.methods[i] = newMethod(ifaceType, i, p.target, tTab, tData)
		fvs[i] = p.methods[i].codePtr
	}

	// The concrete type recorded in the itab is *Proxy, which is exactly what
	// the data word points at, so the collector scans it correctly.
	proxyType := typeOf(reflect.TypeOf(p))
	p.itab = forgeITab(inter, proxyType, fvs)
	return p
}

// As reinterprets the proxy as an interface value of type T. T must be the
// exact interface type the proxy was built for.
//
// This is a free function rather than a method because Go does not allow
// methods to have their own type parameters.
func As[T any](p *Proxy) T {
	var zero T
	makeIface(&zero, p.itab, unsafe.Pointer(p))
	return zero
}

// Type returns the proxied interface type.
func (p *Proxy) Type() reflect.Type { return p.typ }

// Methods returns the descriptors of the proxied methods, in itab slot order.
func (p *Proxy) Methods() []*Method { return p.methods }

// Target returns the object calls are delegated to, or nil for a mock.
func (p *Proxy) Target() any {
	if !p.target.IsValid() {
		return nil
	}
	return p.target.Interface()
}

// String implements fmt.Stringer so that printing a proxy does not try to
// describe the forged itab.
func (p *Proxy) String() string {
	if !p.target.IsValid() {
		return fmt.Sprintf("weave.Proxy[%s](nil)", p.typ.String())
	}
	return fmt.Sprintf("weave.Proxy[%s](%v)", p.typ.String(), p.target)
}

// newMethod builds the per-proxy descriptor for method i of ifaceType. The
// trampoline slot is simply the method index, so no global state is involved
// and no two interfaces compete for slots.
func newMethod(ifaceType reflect.Type, i int, target reflect.Value, tTab *itab, tData unsafe.Pointer) *Method {
	m := &Method{
		Name:  ifaceType.Method(i).Name,
		Type:  ifaceType.Method(i).Type,
		Index: i,
	}
	m.layout = newABILayout(m.Type)

	if target.IsValid() {
		if fn := target.MethodByName(m.Name); fn.IsValid() {
			m.targetFn = fn
		}
	}
	if tTab != nil && m.targetFn.IsValid() {
		m.targetFun = itabFun(tTab, ifaceType.NumMethod(), i)
		m.targetData = tData
	}

	nOut := m.Type.NumOut()
	if nOut > 0 {
		m.zeros = make([]reflect.Value, nOut)
		for k := 0; k < nOut; k++ {
			m.zeros[k] = reflect.Zero(m.Type.Out(k))
		}
	}

	m.codePtr = newTrampoline(m)
	return m
}
