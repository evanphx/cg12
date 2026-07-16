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

// Interp is the dynamic loader a dynamically linked amd64 executable runs under.
func (Backend) Interp() string { return "/lib64/ld-linux-x86-64.so.2" }

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

// StartStub returns a `_start` object: the image's entry point, standing in for a
// C runtime. It calls each init function, then entry, then each fini function,
// and exits with entry's return value via the exit_group syscall -- so a linked
// executable needs no libc startup code.
//
// The loader deliberately does not run an executable's DT_INIT_ARRAY (it leaves
// that to the C runtime, which is us here), so the calls are emitted directly.
func (Backend) StartStub(entry string, init, fini []string) (*obj.Object, error) {
	const exitGroup = 231
	p := x64.NewProgram()
	p.Globl("_start")
	p.Label("_start")
	for _, n := range init {
		p.Call(sanitize(n))
	}
	p.Call(sanitize(entry))
	ret := x64.RAX
	if len(fini) > 0 {
		// entry's result is in eax, which the fini calls would clobber; rbx is
		// callee-saved, so it survives them.
		p.Emit(x64.MovReg(false, x64.RBX, x64.RAX))
		for _, n := range fini {
			p.Call(sanitize(n))
		}
		ret = x64.RBX
	}
	p.Emit(x64.MovReg(false, x64.RDI, ret))         // exit code = entry's result
	p.Emit(x64.MovImm32(false, x64.RAX, exitGroup)) // mov eax, 231
	p.Emit(x64.Syscall())
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
