package arm64

import (
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// Backend exposes the arm64 code generator through the common codegen interface
// so a generic driver (the linker) can consume its object output without
// depending on arm64-specific entry points. Opts carries the same options as
// CompileObjectWith. The zero value is a usable backend with default options.
type Backend struct{ Opts Options }

// Machine reports the ELF machine type of the objects this backend emits.
func (Backend) Machine() uint16 { return obj.EM_AARCH64 }

// CompileModule compiles an IR module to a relocatable object.
func (b Backend) CompileModule(m *ir.Module) (*obj.Object, error) {
	return CompileToObjectWith(m, b.Opts)
}
