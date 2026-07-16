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

// Interp is the dynamic loader a dynamically linked arm64 executable runs under.
func (Backend) Interp() string { return "/lib/ld-linux-aarch64.so.1" }

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
	return programObject(p)
}

// StartStub returns a `_start` object: the image's entry point, standing in for a
// C runtime. It calls each init function, then entry, then each fini function,
// and exits with entry's return value via the exit_group syscall -- so a linked
// executable needs no libc startup code.
//
// The loader deliberately does not run an executable's DT_INIT_ARRAY (it leaves
// that to the C runtime, which is us here), so the calls are emitted directly.
func (Backend) StartStub(entry string, init, fini []string) (*obj.Object, error) {
	const (
		x0, x19   = a64.Reg(0), a64.Reg(19)
		exitGroup = 94
	)
	p := a64.NewProgram()
	p.Globl("_start")
	p.Label("_start")
	for _, n := range init {
		p.Bl(sanitize(n))
	}
	p.Bl(sanitize(entry))
	if len(fini) > 0 {
		// entry's result is in w0, which the fini calls would clobber; x19 is
		// callee-saved, so it survives them.
		p.Emit(a64.MovReg(true, x19, x0))
		for _, n := range fini {
			p.Bl(sanitize(n))
		}
		p.Emit(a64.MovReg(true, x0, x19))
	}
	p.Emit(a64.Movz(true, a64.Reg(8), exitGroup, 0)) // movz x8, #94
	p.Emit(0xd4000001)                               // svc #0
	return programObject(p)
}

// programObject turns an assembled a64 program into a relocatable object: its
// defined labels become .text symbols (globals exported), and references to
// undefined labels become relocations the linker resolves.
func programObject(p *a64.Program) (*obj.Object, error) {
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
