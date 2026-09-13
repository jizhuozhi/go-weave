package main

// One interceptor chain, applied uniformly to any interface.
//
// This is the SPI shape — the interface is the contract, an implementation is
// plugged in — except that the cross-cutting concerns come from the proxy
// rather than from a hand-written decorator. Two unrelated interfaces (Storage,
// Greeter) get the same behaviour, and neither implementation contains a line
// of auth or logging code.
//
// Register is also the reference for the generic-wrapper pattern: T is an
// unconstrained type parameter here, so nil cannot be passed to weave.New[T] —
// it has to go through NewOf/As.
//
// Run: go run ./examples/spi

import (
	"context"
	"fmt"
	"reflect"

	"github.com/jizhuozhi/go-weave"
)

// The contracts. Callers depend on these and never on an implementation.

type Storage interface {
	Get(ctx context.Context, key string) (string, error)
	Put(ctx context.Context, key, value string) error
}

type Greeter interface {
	Hello(ctx context.Context, name string) string
}

// The providers. Each has its own implementation, unknown to the framework and
// to callers.

type memoryStorage struct{ m map[string]string }

func (s *memoryStorage) Get(_ context.Context, key string) (string, error) {
	v, ok := s.m[key]
	if !ok {
		return "", fmt.Errorf("key %q not found", key)
	}
	return v, nil
}

func (s *memoryStorage) Put(_ context.Context, key, value string) error {
	s.m[key] = value
	return nil
}

type englishGreeter struct{}

func (englishGreeter) Hello(_ context.Context, name string) string {
	return "hello, " + name
}

// ctxKey is a defined type used as a context key, so a bare string key cannot
// collide across packages (SA1029).
type ctxKey string

const callerKey ctxKey = "caller"

// Register wraps an implementation in the shared chain and hands it back as the
// interface. The framework never learns which methods T has.
func Register[T any](impl T) T {
	iface := reflect.TypeOf((*T)(nil)).Elem()
	proxy := weave.NewOf(iface, impl, auth, trace)
	return weave.As[T](proxy)
}

// auth short-circuits: with no caller in the context the method is never
// reached, and the interceptor supplies the zero results itself.
func auth(c *weave.Invocation) []reflect.Value {
	ctx := c.Arg(0).Interface().(context.Context)
	caller, _ := ctx.Value(callerKey).(string)
	if caller == "" {
		fmt.Printf("auth: DENY %s (no caller)\n", c.Method.Name)
		return zeroResults(c)
	}
	fmt.Printf("auth: allow %s as %s\n", c.Method.Name, caller)
	return c.Proceed()
}

// trace is around advice: it resumes after the rest of the chain has returned.
func trace(c *weave.Invocation) []reflect.Value {
	res := c.Proceed()
	fmt.Printf("trace: %s done\n", c.Method.Name)
	return res
}

func zeroResults(c *weave.Invocation) []reflect.Value {
	out := make([]reflect.Value, c.Method.NumOut())
	for i := range out {
		out[i] = reflect.Zero(c.Method.Type.Out(i))
	}
	return out
}

func main() {
	// Each provider registers itself; callers only ever see the interface.
	storage := Register[Storage](&memoryStorage{m: map[string]string{"name": "ada"}})
	greeter := Register[Greeter](englishGreeter{})

	ctx := context.WithValue(context.Background(), callerKey, "alice")

	name, err := storage.Get(ctx, "name")
	fmt.Println("got:", name, err)

	fmt.Println(greeter.Hello(ctx, "bob"))

	// No caller in the context: auth denies, so Put never runs.
	_ = storage.Put(context.Background(), "hack", "x")
}
