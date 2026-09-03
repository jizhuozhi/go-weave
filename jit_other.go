//go:build !darwin

package weave

// jitWriteProtect is a no-op off Darwin: MAP_JIT and its write-protection
// toggle exist only on Apple Silicon.
func jitWriteProtect(on bool) {}

// jitFlushICache is a no-op off Apple Silicon.
func jitFlushICache(addr, n uintptr) {}
