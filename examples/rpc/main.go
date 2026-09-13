// An RPC stub: the interface is the wire contract, the interceptor does the
// serialization and the transport.
//
// The caller holds an ordinary interface value and the server is an ordinary
// struct — neither knows weave exists, and no stub code was generated. Swap
// transport.invoke for a real TCP/HTTP round trip and this is the shape you
// would ship.
//
// Run:
//
//	cd examples/rpc && go run .
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jizhuozhi/go-weave"
)

// The wire contract: the only thing caller and server agree on.

type Calculator interface {
	Add(ctx context.Context, a, b int) (int, error)
	Div(ctx context.Context, a, b int) (int, error)
}

// The server: an ordinary implementation that knows nothing about weave.

type calculator struct{ calls int }

func (c *calculator) Add(_ context.Context, a, b int) (int, error) {
	c.calls++
	return a + b, nil
}

func (c *calculator) Div(_ context.Context, a, b int) (int, error) {
	c.calls++
	if b == 0 {
		return 0, fmt.Errorf("division by zero")
	}
	return a / b, nil
}

// A minimal transport.
//
// frame is what actually travels on the wire. Write it to a connection instead
// and nothing else on either side changes.

type frame struct {
	Method string            `json:"method"`
	Args   []json.RawMessage `json:"args"`
}

type transport struct{ impl reflect.Value }

func newTransport(impl any) *transport { return &transport{impl: reflect.ValueOf(impl)} }

var (
	ctxType = reflect.TypeOf((*context.Context)(nil)).Elem()
	errType = reflect.TypeOf((*error)(nil)).Elem()
)

func isContext(t reflect.Type) bool { return t.Implements(ctxType) }

// invoke plays the far side: it receives a frame, reflect-calls the real
// implementation and serialises the results back. In a real system this
// function lives on another machine and is the only part that needs the ctx.
func (t *transport) invoke(f frame) []json.RawMessage {
	m := t.impl.MethodByName(f.Method)
	if !m.IsValid() {
		panic("rpc: no such method " + f.Method)
	}
	in := make([]reflect.Value, m.Type().NumIn())
	next := 0
	for i := range in {
		if isContext(m.Type().In(i)) {
			in[i] = reflect.ValueOf(context.Background())
			continue
		}
		p := reflect.New(m.Type().In(i))
		if err := json.Unmarshal(f.Args[next], p.Interface()); err != nil {
			panic(err)
		}
		in[i] = p.Elem()
		next++
	}
	out := m.Call(in)
	results := make([]json.RawMessage, len(out))
	for i, v := range out {
		// error is an interface: json.Marshal would produce {} and lose the
		// message entirely. Encode it as text instead.
		if v.Type() == errType {
			if v.IsNil() {
				results[i] = json.RawMessage("null")
			} else {
				raw, _ := json.Marshal(v.Interface().(error).Error())
				results[i] = raw
			}
			continue
		}
		raw, err := json.Marshal(v.Interface())
		if err != nil {
			panic(err)
		}
		results[i] = raw
	}
	return results
}

// stub turns the interface into a stub: the interface is the declaration, the
// interceptor is the implementation.
//
// There is no generated implementation of Calculator and no generated wire
// struct. c.Method.Name is the method the far side must call, and the arguments
// are encoded one by one from the interface signature — all of it comes from
// reflection.
//
// T is an unconstrained type parameter here, so nil cannot be assigned to it;
// hence the NewOf/As pair. This is the general shape for any generic wrapper
// (see Register in examples/spi).
func stub[T any](t *transport) T {
	iface := reflect.TypeOf((*T)(nil)).Elem()
	proxy := weave.NewOf(iface, nil, func(c *weave.Invocation) []reflect.Value {
		f := frame{Method: c.Method.Name}
		for _, a := range c.Args() {
			if isContext(a.Type()) {
				continue // ctx does not cross the wire; the far side makes its own
			}
			raw, err := json.Marshal(a.Interface())
			if err != nil {
				return fail(c, err)
			}
			f.Args = append(f.Args, raw)
		}
		fmt.Printf("  --> %s(%s)\n", f.Method, joinRaw(f.Args))

		results := t.invoke(f)

		out := make([]reflect.Value, c.Method.NumOut())
		for i, raw := range results {
			ret := c.Method.Type.Out(i)
			// The matching decode: a string on the wire becomes an error, null
			// becomes nil.
			if ret == errType {
				if string(raw) == "null" {
					out[i] = reflect.Zero(ret)
					continue
				}
				var msg string
				if err := json.Unmarshal(raw, &msg); err != nil {
					return fail(c, err)
				}
				out[i] = reflect.ValueOf(errors.New(msg))
				continue
			}
			p := reflect.New(ret)
			if err := json.Unmarshal(raw, p.Interface()); err != nil {
				return fail(c, err)
			}
			out[i] = p.Elem()
		}
		fmt.Printf("  <-- %s\n", joinRaw(results))
		return out
	})
	return weave.As[T](proxy)
}

// fail builds a "zero results plus error" return; the shape has to match the
// method's signature.
func fail(c *weave.Invocation, err error) []reflect.Value {
	out := make([]reflect.Value, c.Method.NumOut())
	for i := range out {
		out[i] = reflect.Zero(c.Method.Type.Out(i))
	}
	out[len(out)-1] = reflect.ValueOf(err)
	return out
}

func joinRaw(raws []json.RawMessage) string {
	parts := make([]string, len(raws))
	for i, r := range raws {
		parts[i] = string(r)
	}
	return strings.Join(parts, ", ")
}

func main() {
	impl := &calculator{}
	remote := newTransport(impl)

	// The caller depends only on the interface; it cannot even tell whether the
	// far side is a local object or a remote process.
	var calc Calculator = stub[Calculator](remote)

	ctx := context.Background()

	sum, err := calc.Add(ctx, 2, 3)
	fmt.Printf("Add(2, 3)  = %v, %v\n\n", sum, err)

	quot, err := calc.Div(ctx, 10, 4)
	fmt.Printf("Div(10, 4) = %v, %v\n\n", quot, err)

	_, err = calc.Div(ctx, 1, 0)
	fmt.Printf("Div(1, 0)  = error: %v\n\n", err)

	fmt.Printf("the server handled %d calls\n", impl.calls)
}
