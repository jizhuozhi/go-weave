// Package rt holds the tiny amount of platform shim the JIT trampoline needs:
// toggling MAP_JIT writability on Apple Silicon. Everything else is pure Go in
// the parent package; this file is the cross-platform interface.
package rt

// JitWriteProtect toggles writability of MAP_JIT pages. It is a no-op except on
// darwin/arm64, where executable pages start RX and pthread_jit_write_protect_np
// switches them to RW for writing and back to RX for execution.
func JitWriteProtect(on bool) {
	jitWriteProtect(on)
}
