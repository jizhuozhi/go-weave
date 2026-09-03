//go:build darwin

package weave

// jitWriteProtect is stubbed for now: toggling MAP_JIT writability on Apple
// Silicon needs pthread_jit_write_protect_np, which has no runtime trampoline
// and must go through a cgo shim in a separate package (cgo cannot coexist
// with the .s files in this package). See the note in jit_test.go.
func jitWriteProtect(on bool) {}
