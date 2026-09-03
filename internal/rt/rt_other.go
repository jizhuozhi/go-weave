//go:build !darwin || !arm64

package rt

func jitWriteProtect(on bool) {}

// FlushICache is a no-op off Apple Silicon; other platforms do not require an
// explicit instruction-cache invalidate after writing code.
func FlushICache(addr uintptr, n uintptr) {}

// Selftest reports success off Apple Silicon; only darwin/arm64 has the
// MAP_JIT write-protect mechanism to validate.
func Selftest() bool { return true }
