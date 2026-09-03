//go:build !go1.20

package weave

// rfunc mirrors runtime._func (Go 1.18/1.19: no startLine yet), minus the
// trailing variable arrays.
type rfunc struct {
	entryOff    uint32
	nameOff     int32
	args        int32
	deferreturn uint32
	pcsp        uint32
	pcfile      uint32
	pcln        uint32
	npcdata     uint32
	cuOffset    uint32
	funcID      uint8
	flag        uint8
	_           [1]byte
	nfuncdata   uint8
}
