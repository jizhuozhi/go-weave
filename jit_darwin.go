//go:build darwin

package weave

import "github.com/jizhuozhi/go-weave/internal/rt"

// jitWriteProtect toggles writability of MAP_JIT pages; see internal/rt.
func jitWriteProtect(on bool) { rt.JitWriteProtect(on) }

// jitFlushICache invalidates the instruction cache after writing machine code.
func jitFlushICache(addr, n uintptr) { rt.FlushICache(addr, n) }
