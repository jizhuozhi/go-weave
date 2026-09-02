//go:build arm64

package weave

// Values from internal/abi/abi_arm64.go.
const (
	intArgRegs   = 16 // R0 - R15
	floatArgRegs = 16 // F0 - F15
	floatRegSize = 8
)
