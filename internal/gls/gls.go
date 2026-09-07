// Package gls provides the foundation of goroutine-local storage: a unique
// identifier for the current goroutine. It exposes only the g pointer itself
// (no struct mirroring), so it is version-free; upper frameworks use it as a
// goroutine-local map key.
package gls

import "unsafe"

// getg returns the current goroutine's g pointer; see getg_amd64.s / getg_arm64.s.
func getg() unsafe.Pointer

// Key returns a unique identifier for the current goroutine (the g pointer),
// usable as a goroutine-local map key.
func Key() uintptr { return uintptr(getg()) }
