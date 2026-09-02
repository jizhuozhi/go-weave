//go:build amd64

package weave

// Values from internal/abi/abi_amd64.go.
const (
	intArgRegs   = 9  // AX BX CX DI SI R8 R9 R10 R11
	floatArgRegs = 15 // X0 - X14
	floatRegSize = 8
)
