//go:build arm64

package weave

// Values from internal/abi/abi_arm64.go.
const (
	intArgRegs   = 16 // R0-R15
	floatArgRegs = 16 // F0-F15
	floatRegSize = 8

	// stackWindow is the number of bytes of a method's stack argument area
	// (arguments plus results) the generated trampoline can capture through
	// its trailing stack parameter.
	stackWindow = 480
)
