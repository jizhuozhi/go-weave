//go:build !arm64 && !amd64

package weave

// Registers are only described for the architectures that ship the generated
// trampoline; every other platform is rejected at proxy construction.
const (
	intArgRegs   = 0
	floatArgRegs = 0
	floatRegSize = 0
	stackWindow  = 0
)
