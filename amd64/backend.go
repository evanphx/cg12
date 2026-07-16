package amd64

import (
	"sort"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// Backend exposes the amd64 code generator through the common codegen interface
// so a generic driver (the linker) can consume its object output without
// depending on amd64-specific entry points. Opts carries the same options as
// CompileObjectWith. The zero value is a usable backend with default options.
type Backend struct{ Opts Options }

// Machine reports the ELF machine type of the objects this backend emits.
func (Backend) Machine() uint16 { return obj.EM_X86_64 }

// CompileModule compiles an IR module to a relocatable object.
func (b Backend) CompileModule(m *ir.Module) (*obj.Object, error) {
	return CompileToObjectWith(m, b.Opts)
}

// Assemble turns hand-written AT&T assembly text into a relocatable object: the
// program's defined labels become .text symbols (those named in a `.globl`
// directive are exported), and calls or references to undefined labels become
// relocations the linker resolves. This is the "own linking" path -- machine
// code produced without an external assembler.
func (Backend) Assemble(src string) (*obj.Object, error) {
	p, err := x64.AssembleProgram(src)
	if err != nil {
		return nil, err
	}
	return programObject(p)
}

// StartStub returns a `_start` object that calls entry and exits with its return
// value via the exit_group syscall, so a fully linked executable needs no C
// runtime. The entry function's return value comes back in eax.
func (Backend) StartStub(entry string) (*obj.Object, error) {
	p := x64.NewProgram()
	p.Globl("_start")
	p.Label("_start")
	p.Call(sanitize(entry))                     // call entry (PLT32 to the entry function)
	p.Emit(x64.MovReg(false, x64.RDI, x64.RAX)) // mov edi, eax (exit code = return value)
	p.Emit(x64.MovImm32(false, x64.RAX, 231))   // mov eax, 231 (__NR_exit_group)
	p.Emit(x64.Syscall())                       // syscall
	return programObject(p)
}

// programObject turns an assembled x64 program into a relocatable object: its
// defined labels become .text symbols (globals exported), and references to
// undefined labels become relocations the linker resolves.
func programObject(p *x64.Program) (*obj.Object, error) {
	code, relocs, err := p.Link()
	if err != nil {
		return nil, err
	}
	o := &obj.Object{Machine: obj.EM_X86_64, Text: code}

	// Emit defined labels as symbols in a stable (offset, name) order.
	type lbl struct {
		name string
		off  int
	}
	var labels []lbl
	for n, off := range p.Labels() {
		labels = append(labels, lbl{n, off})
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
			Offset: uint64(r.Offset), Sym: r.Sym, Type: relType(r.Kind), Addend: r.Addend,
		})
	}
	return o, nil
}

// relType maps an assembler relocation class to its ELF x86-64 type.
func relType(k x64.RelKind) uint32 {
	if k == x64.RelPLT32 {
		return obj.R_X86_64_PLT32
	}
	return obj.R_X86_64_PC32
}
