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
	code   []byte
	labels map[string]int
	fixups []fixup
	err    error
}

// fixup is an unresolved disp32 at byte offset `at`. The value stored is
// (target - ref), where ref is `end` (the instruction end, for RIP-relative
// branches and leas) unless `base` names a label to measure from (for a
// jump-table data word).
type fixup struct {
	at     int
	end    int
	target string
	base   string
}

// NewProgram returns an empty program.
func NewProgram() *Program {
	return &Program{labels: map[string]int{}}
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

// Jmp emits JMP to a label (E9 cd) with a patched-later displacement.
func (p *Program) Jmp(label string) { p.branch([]byte{0xe9}, label) }

// Jcc emits Jcc to a label (0F 80+cc cd).
func (p *Program) Jcc(c Cond, label string) { p.branch([]byte{0x0f, 0x80 | byte(c)}, label) }

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
	p.Emit(opcode)
	at := len(p.code)
	p.code = append(p.code, 0, 0, 0, 0)
	p.fixups = append(p.fixups, fixup{at: at, end: len(p.code), target: label})
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
