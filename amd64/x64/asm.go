package x64

import "fmt"

// Program accumulates encoded instructions into a byte buffer and resolves
// intra-function branches to labels. Because x86-64 instructions are variable
// length, positions are tracked as byte offsets (not word indices): a label
// records the current byte length, and a branch to a label emits its opcode plus
// a placeholder disp32 that Bytes patches once every label position is known.
//
// Symbol-level references (calls to other functions, RIP-relative data loads)
// are not resolved here — the backend records those as object relocations at the
// instruction offsets Program hands back.
type Program struct {
	code    []byte
	labels  map[string]int
	globals map[string]bool
	fixups  []fixup
	err     error
}

// RelKind is the class of an unresolved reference: a call binds through the PLT
// (R_X86_64_PLT32), any other PC-relative use (jmp/jcc/lea) is R_X86_64_PC32.
type RelKind int

const (
	RelPCRel32 RelKind = iota
	RelPLT32
)

// Reloc is a reference the assembler could not bind to a local label; the final
// linker must supply the symbol's address. Offset is the byte position of the
// 4-byte displacement field and Addend already folds in the -4 instruction-end
// bias, so the resolved value is sym + Addend - Offset.
type Reloc struct {
	Offset int
	Sym    string
	Kind   RelKind
	Addend int64
}

// fixup is an unresolved disp32 at byte offset `at`. The value stored is
// (target - ref), where ref is `end` (the instruction end, for RIP-relative
// branches and leas) unless `base` names a label to measure from (for a
// jump-table data word). kind selects the relocation emitted when target is not
// a local label.
type fixup struct {
	at     int
	end    int
	target string
	base   string
	kind   RelKind
}

// NewProgram returns an empty program.
func NewProgram() *Program {
	return &Program{labels: map[string]int{}, globals: map[string]bool{}}
}

// Len returns the current byte length, i.e. the offset of the next instruction.
func (p *Program) Len() int { return len(p.code) }

// LabelOffset returns a label's byte offset, and whether it is defined.
func (p *Program) LabelOffset(name string) (int, bool) {
	i, ok := p.labels[name]
	return i, ok
}

// Emit appends raw encoded bytes and returns the offset at which they began.
func (p *Program) Emit(b []byte) int {
	off := len(p.code)
	p.code = append(p.code, b...)
	return off
}

// Label records that name refers to the current offset.
func (p *Program) Label(name string) {
	if _, dup := p.labels[name]; dup {
		p.setErr(fmt.Errorf("x64: duplicate label %q", name))
		return
	}
	p.labels[name] = len(p.code)
}

// Globl marks a label as a global (exported) symbol in the assembled object.
func (p *Program) Globl(name string) { p.globals[name] = true }

// Jmp emits JMP to a label (E9 cd) with a patched-later displacement.
func (p *Program) Jmp(label string) { p.branch([]byte{0xe9}, label) }

// Jcc emits Jcc to a label (0F 80+cc cd).
func (p *Program) Jcc(c Cond, label string) { p.branch([]byte{0x0f, 0x80 | byte(c)}, label) }

// Call emits CALL to a label (E8 cd). An undefined target becomes a PLT32
// relocation, so hand-written assembly can call other functions by name.
func (p *Program) Call(label string) {
	p.branchKind([]byte{0xe8}, label, RelPLT32)
}

// LeaLabel emits LEA dst, [rip + label]: it materializes a label's address
// (RIP-relative), patching the disp32 like a branch does.
func (p *Program) LeaLabel(w bool, dst Reg, label string) {
	code := Lea(w, dst, RIPRel(0)) // disp32 placeholder is the last 4 bytes
	p.Emit(code)
	p.fixups = append(p.fixups, fixup{at: len(p.code) - 4, end: len(p.code), target: label})
}

// DataWord emits a 32-bit data word (never executed) equal to the signed byte
// distance from base to target -- a jump-table offset entry.
func (p *Program) DataWord(target, base string) {
	at := len(p.code)
	p.code = append(p.code, 0, 0, 0, 0)
	p.fixups = append(p.fixups, fixup{at: at, end: at, target: target, base: base})
}

// branch emits an opcode followed by a 4-byte displacement placeholder and
// records a fixup so Bytes can fill in (labelOffset - instructionEnd).
func (p *Program) branch(opcode []byte, label string) {
	p.branchKind(opcode, label, RelPCRel32)
}

// branchKind is branch with an explicit relocation class for the case where the
// target is not a local label.
func (p *Program) branchKind(opcode []byte, label string, kind RelKind) {
	p.Emit(opcode)
	at := len(p.code)
	p.code = append(p.code, 0, 0, 0, 0)
	p.fixups = append(p.fixups, fixup{at: at, end: len(p.code), target: label, kind: kind})
}

func (p *Program) setErr(err error) {
	if p.err == nil {
		p.err = err
	}
}

// Bytes resolves every label fixup and returns the finished machine code.
func (p *Program) Bytes() ([]byte, error) {
	if p.err != nil {
		return nil, p.err
	}
	for _, f := range p.fixups {
		target, ok := p.labels[f.target]
		if !ok {
			return nil, fmt.Errorf("x64: undefined label %q", f.target)
		}
		ref := f.end
		if f.base != "" {
			b, ok := p.labels[f.base]
			if !ok {
				return nil, fmt.Errorf("x64: undefined base label %q", f.base)
			}
			ref = b
		}
		rel := int32(target - ref)
		b := p.code[f.at:]
		b[0] = byte(rel)
		b[1] = byte(rel >> 8)
		b[2] = byte(rel >> 16)
		b[3] = byte(rel >> 24)
	}
	return p.code, nil
}

// Link resolves every fixup whose target is a local label (as Bytes does) and
// returns the code together with a relocation for each fixup that references an
// external symbol, so hand-written assembly calling other functions can be
// turned into a relocatable object. A jump-table base label must always be
// local; an undefined one is an error.
func (p *Program) Link() ([]byte, []Reloc, error) {
	if p.err != nil {
		return nil, nil, p.err
	}
	var relocs []Reloc
	for _, f := range p.fixups {
		target, ok := p.labels[f.target]
		if !ok {
			if f.base != "" {
				return nil, nil, fmt.Errorf("x64: undefined base label %q", f.base)
			}
			relocs = append(relocs, Reloc{
				Offset: f.at, Sym: f.target, Kind: f.kind, Addend: int64(f.at - f.end),
			})
			continue
		}
		ref := f.end
		if f.base != "" {
			b, ok := p.labels[f.base]
			if !ok {
				return nil, nil, fmt.Errorf("x64: undefined base label %q", f.base)
			}
			ref = b
		}
		rel := int32(target - ref)
		b := p.code[f.at:]
		b[0], b[1], b[2], b[3] = byte(rel), byte(rel>>8), byte(rel>>16), byte(rel>>24)
	}
	return p.code, relocs, nil
}

// Labels returns the defined labels and their byte offsets.
func (p *Program) Labels() map[string]int { return p.labels }

// IsGlobal reports whether a label was marked global with Globl.
func (p *Program) IsGlobal(name string) bool { return p.globals[name] }
