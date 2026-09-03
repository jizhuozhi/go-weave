//go:build (amd64 || arm64) && !go1.23

package weave

import "unsafe"

// jitTrampoline is unavailable before Go 1.23: the runtime.moduledata layout it
// forges differs on older releases, so proxy construction falls back to the
// compile-time precise trampolines (StubSource).
func jitTrampoline(sh stubShape) unsafe.Pointer {
	return nil
}
