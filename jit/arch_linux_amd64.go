package jit

import (
	"fmt"
	"math"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

const machine = obj.EM_X86_64

// Compile compiles a cg12 module to an object this package can load.
func Compile(m *ir.Module) (*obj.Object, error) {
	onGoThread(m)
	return amd64.CompileToObject(m)
}

// x86-64 keeps its instruction cache coherent with stores.
func flushICache(addr, n uintptr) {}

// relocateArch patches the relocation at the start of b, whose address
// is p, to refer to v, the symbol's address plus the addend.
func relocateArch(b []byte, p, v int64, typ uint32) error {
	switch typ {
	case obj.R_X86_64_64:
		return put64(b, uint64(v))
	case obj.R_X86_64_PC32, obj.R_X86_64_PLT32:
		if !fits(v-p, 32) {
			return errRange
		}
		put32(b, uint32(v-p))
	case obj.R_X86_64_32:
		if v < 0 || v > math.MaxUint32 {
			return errRange
		}
		put32(b, uint32(v))
	case obj.R_X86_64_32S:
		if !fits(v, 32) {
			return errRange
		}
		put32(b, uint32(v))
	default:
		return fmt.Errorf("type %d is unsupported", typ)
	}
	return nil
}
