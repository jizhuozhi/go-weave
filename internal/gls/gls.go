// Package gls 提供 goroutine-local storage 的基础：当前 goroutine 的唯一标识。
// 它只暴露 g 指针本身（不做任何结构体镜像），因此零版本分段，可被上层框架
// 拿来做 goroutine-local 的 map key。
package gls

import "unsafe"

// getg 返回当前 goroutine 的 g 指针，见 getg_amd64.s / getg_arm64.s：
// amd64 从 TLS 读，arm64 从 g 寄存器读。
func getg() unsafe.Pointer

// Key 返回当前 goroutine 的唯一标识（g 指针值），可用作 goroutine-local map 的 key。
func Key() uintptr { return uintptr(getg()) }
