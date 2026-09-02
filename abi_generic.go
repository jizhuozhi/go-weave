//go:build !arm64 && !amd64

package weave

// Registers are only described for the architectures that ship an assembly
// trampoline. Everything else still works through the reflect backend, which
// needs none of this.
const (
	intArgRegs   = 0
	floatArgRegs = 0
	floatRegSize = 0
)
