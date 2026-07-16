package arm64

import (
	"sort"

	"github.com/evanphx/cg12/arm64/a64"
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

// Assemble turns hand-written AArch64 assembly text into a relocatable object:
// the program's defined labels become .text symbols (those named in a `.globl`
// directive are exported), and bl/b to undefined labels become CALL26/JUMP26
// relocations the linker resolves. This is the "own linking" path -- machine
// code produced without an external assembler.
func (Backend) Assemble(src string) (*obj.Object, error) {
	p, err := a64.AssembleProgram(src)
	if err != nil {
		return nil, err
	}
	code, relocs, err := p.Link()
	if err != nil {
		return nil, err
	}
	o := &obj.Object{Machine: obj.EM_AARCH64, Text: code}

	// Emit defined labels as symbols in a stable (offset, name) order. A64 label
	// offsets are word indices, so scale by 4 for the byte value.
	type lbl struct {
		name string
		off  int
	}
	var labels []lbl
	for n, word := range p.Labels() {
		labels = append(labels, lbl{n, word * 4})
	}
	sort.Slice(labels, func(i, j int) bool {
		if labels[i].off != labels[j].off {
			return labels[i].off < labels[j].off
		}
		return labels[i].name < labels[j].name
	})
	for _, l := range labels {
		o.Syms = append(o.Syms, obj.Sym{
			Name: l.name, Section: obj.SecText, Value: uint64(l.off),
			Global: p.IsGlobal(l.name), Func: p.IsGlobal(l.name),
		})
	}
	for _, r := range relocs {
		o.Relocs = append(o.Relocs, obj.Reloc{
			Offset: uint64(r.Offset), Sym: r.Sym, Type: aarch64RelType(r.Kind),
		})
	}
	return o, nil
}

// aarch64RelType maps an assembler relocation class to its ELF AArch64 type.
func aarch64RelType(k a64.RelKind) uint32 {
	if k == a64.RelCall26 {
		return obj.R_AARCH64_CALL26
	}
	return obj.R_AARCH64_JUMP26
}
