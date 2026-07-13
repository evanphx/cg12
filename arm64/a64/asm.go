package a64

import "fmt"

// Program assembles a stream of A64 instructions into machine code, resolving
// branches to labels. Instructions are emitted in order; a branch to a label is
// recorded as a fixup and patched once every label position is known. The result
// is a flat little-endian byte slice ready to place in a .text section.
type Program struct {
	words  []uint32
	labels map[string]int // label -> word index
	fixups []fixup
	err    error
}

type fixup struct {
	at    int                    // word index of the branch to patch
	label string                 // target label
	enc   func(off int32) uint32 // re-encode the branch given a byte offset
	kind  string                 // for range-check diagnostics
	bits  uint                   // signed offset width (imm bits + 2 for the /4 scale)
}

// NewProgram returns an empty program.
func NewProgram() *Program {
	return &Program{labels: map[string]int{}}
}

// Emit appends one fully-encoded instruction word.
func (p *Program) Emit(word uint32) { p.words = append(p.words, word) }

// Label marks the current position with a name, to be referenced by branches.
func (p *Program) Label(name string) {
	if _, dup := p.labels[name]; dup && p.err == nil {
		p.err = fmt.Errorf("a64: duplicate label %q", name)
	}
	p.labels[name] = len(p.words)
}

// Len returns the number of instructions emitted so far.
func (p *Program) Len() int { return len(p.words) }

func (p *Program) branch(label string, bits uint, kind string, enc func(int32) uint32) {
	p.fixups = append(p.fixups, fixup{at: len(p.words), label: label, enc: enc, kind: kind, bits: bits})
	p.words = append(p.words, 0) // placeholder, patched in Bytes
}

// B / Bl / Bcond / Cbz / Cbnz emit a branch to a label.
func (p *Program) B(label string)  { p.branch(label, 28, "b", B) }
func (p *Program) Bl(label string) { p.branch(label, 28, "bl", Bl) }
func (p *Program) Bcond(c Cond, label string) {
	p.branch(label, 21, "b.cond", func(off int32) uint32 { return Bcond(c, off) })
}
func (p *Program) Cbz(w64 bool, rt Reg, label string) {
	p.branch(label, 21, "cbz", func(off int32) uint32 { return Cbz(w64, rt, off) })
}
func (p *Program) Cbnz(w64 bool, rt Reg, label string) {
	p.branch(label, 21, "cbnz", func(off int32) uint32 { return Cbnz(w64, rt, off) })
}

// Adr materializes a label's PC-relative address into rd (an ADR fixup: a 21-bit
// signed byte offset, not scaled by 4).
func (p *Program) Adr(rd Reg, label string) {
	p.branch(label, 21, "adr", func(off int32) uint32 { return Adr(rd, off) })
}

// Bytes resolves every branch and returns the assembled machine code.
func (p *Program) Bytes() ([]byte, error) {
	if p.err != nil {
		return nil, p.err
	}
	for _, fx := range p.fixups {
		target, ok := p.labels[fx.label]
		if !ok {
			return nil, fmt.Errorf("a64: undefined label %q", fx.label)
		}
		off := int32(target-fx.at) * 4
		// The signed offset must fit the branch's field (imm bits + 2 scale bits).
		lim := int32(1) << (fx.bits - 1)
		if off < -lim || off >= lim {
			return nil, fmt.Errorf("a64: %s to %q is %d bytes, out of range", fx.kind, fx.label, off)
		}
		p.words[fx.at] = fx.enc(off)
	}
	out := make([]byte, 4*len(p.words))
	for i, w := range p.words {
		out[4*i+0] = byte(w)
		out[4*i+1] = byte(w >> 8)
		out[4*i+2] = byte(w >> 16)
		out[4*i+3] = byte(w >> 24)
	}
	return out, nil
}
