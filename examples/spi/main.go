package main

// SPI + 极简 Spring：接口是 SPI 契约，实现是服务提供者，Register 扮演
// ServiceLoader 的"发现 + 加载"，同时把统一切面（鉴权 → 遥测）织进去——
// 合起来就是 Spring 的 IoC + AOP 的最小形态，只是没有注解、XML、codegen。
// 运行：go run ./examples/spi

import (
	"context"
	"fmt"
	"reflect"

	"github.com/jizhuozhi/go-weave"
)

// ===== SPI 契约：调用方只依赖接口，不依赖实现 =====

type Storage interface {
	Get(ctx context.Context, key string) (string, error)
	Put(ctx context.Context, key, value string) error
}

type Greeter interface {
	Hello(ctx context.Context, name string) string
}

// ===== 服务提供者：各自的实现，框架与调用方都不认识 =====

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

// ===== 容器：注册即代理（ServiceLoader + Spring AOP）=====

// Register 把实现加载成 SPI 服务，返回被统一切面增强的接口代理。一个函数
// 干了三件事：服务发现（ServiceLoader.load）、bean 注册（@Service）、自动
// 代理（Spring AOP）。框架完全不知道 T 有哪些方法。
func Register[T any](impl T) T {
	iface := reflect.TypeOf((*T)(nil)).Elem()
	proxy := weave.NewOf(iface, impl, auth, trace)
	return weave.As[T](proxy)
}

func auth(c *weave.Invocation) []reflect.Value {
	ctx := c.Arg(0).Interface().(context.Context)
	caller, _ := ctx.Value("caller").(string)
	if caller == "" {
		fmt.Printf("auth: DENY %s (no caller)\n", c.Method.Name)
		return zeroResults(c)
	}
	fmt.Printf("auth: allow %s as %s\n", c.Method.Name, caller)
	return c.Proceed()
}

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
	// 服务提供者各自注册；调用方拿到的只是 SPI 接口，不关心实现。
	storage := Register[Storage](&memoryStorage{m: map[string]string{"name": "ada"}})
	greeter := Register[Greeter](englishGreeter{})

	ctx := context.WithValue(context.Background(), "caller", "alice")

	name, err := storage.Get(ctx, "name")
	fmt.Println("got:", name, err)

	fmt.Println(greeter.Hello(ctx, "bob"))

	_ = storage.Put(context.Background(), "hack", "x")
}
