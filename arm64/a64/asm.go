package a64

import "fmt"

// Program assembles a stream of A64 instructions into machine code, resolving
// branches to labels. Instructions are emitted in order; a branch to a label is
// recorded as a fixup and patched once every label position is known. The result
// is a flat little-endian byte slice ready to place in a .text section.
type Program struct {
	words   []uint32
	labels  map[string]int // label -> word index
	globals map[string]bool
	fixups  []fixup
	err     error
}

// RelKind classifies a branch that may reference an external symbol: bl is an
// R_AARCH64_CALL26, b is an R_AARCH64_JUMP26. Other branch forms (conditional,
// cbz, adr) are RelNone -- an undefined target is an error, since the linker has
// no relocation for them.
type RelKind int

const (
	RelNone RelKind = iota
	RelCall26
	RelJump26
)

// Reloc is a branch the assembler could not bind to a local label; the linker
// supplies the target's address. Offset is the byte position of the branch word.
type Reloc struct {
	Offset int
	Sym    string
	Kind   RelKind
}

type fixup struct {
	at    int                    // word index of the branch/data word to patch
	label string                 // target label
	base  string                 // reference label the offset is measured from (empty: fx.at)
	enc   func(off int32) uint32 // re-encode the branch given a byte offset
	kind  string                 // for range-check diagnostics
	bits  uint                   // signed offset width (imm bits + 2 for the /4 scale)
	data  bool                   // emit the raw byte offset as a data word (no encoding)
	rel   RelKind                // relocation class when label is external
}

// NewProgram returns an empty program.
func NewProgram() *Program {
	return &Program{labels: map[string]int{}, globals: map[string]bool{}}
}

// Globl marks a label as a global (exported) symbol in the assembled object.
func (p *Program) Globl(name string) { p.globals[name] = true }

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

// LabelOffset returns a label's byte offset in the assembled code, and whether
// it is defined. Each instruction is 4 bytes.
func (p *Program) LabelOffset(name string) (int, bool) {
	i, ok := p.labels[name]
	return i * 4, ok
}

func (p *Program) branch(label string, bits uint, kind string, enc func(int32) uint32) {
	p.branchRel(label, bits, kind, RelNone, enc)
}

// branchRel is branch with an explicit relocation class for an external target.
func (p *Program) branchRel(label string, bits uint, kind string, rel RelKind, enc func(int32) uint32) {
	p.fixups = append(p.fixups, fixup{at: len(p.words), label: label, enc: enc, kind: kind, bits: bits, rel: rel})
	p.words = append(p.words, 0) // placeholder, patched in Bytes
}

// B / Bl / Bcond / Cbz / Cbnz emit a branch to a label.
func (p *Program) B(label string)  { p.branchRel(label, 28, "b", RelJump26, B) }
func (p *Program) Bl(label string) { p.branchRel(label, 28, "bl", RelCall26, Bl) }
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

// DataWord emits a 32-bit data word (occupying one instruction slot, never
// executed) equal to the signed byte distance from base to target, resolved at
// assembly time. It is the offset entry of a PC-relative jump table.
func (p *Program) DataWord(target, base string) {
	p.fixups = append(p.fixups, fixup{at: len(p.words), label: target, base: base, data: true, kind: "jumptable"})
	p.words = append(p.words, 0)
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
		ref := fx.at
		if fx.base != "" {
			b, ok := p.labels[fx.base]
			if !ok {
				return nil, fmt.Errorf("a64: undefined base label %q", fx.base)
			}
			ref = b
		}
		off := int32(target-ref) * 4
		if fx.data {
			p.words[fx.at] = uint32(off) // raw offset entry, not an instruction
			continue
		}
		// The signed offset must fit the branch's field (imm bits + 2 scale bits).
		lim := int32(1) << (fx.bits - 1)
		if off < -lim || off >= lim {
			return nil, fmt.Errorf("a64: %s to %q is %d bytes, out of range", fx.kind, fx.label, off)
		}
		p.words[fx.at] = fx.enc(off)
	}
	return p.words2bytes(), nil
}

// Link resolves every branch to a local label (as Bytes does) and returns the
// code together with a relocation for each bl/b whose target is an external
// symbol, so hand-written assembly calling other functions can be turned into a
// relocatable object. A conditional/cbz/adr or jump-table reference must resolve
// locally; an undefined one is an error.
func (p *Program) Link() ([]byte, []Reloc, error) {
	if p.err != nil {
		return nil, nil, p.err
	}
	var relocs []Reloc
	for _, fx := range p.fixups {
		target, ok := p.labels[fx.label]
		if !ok {
			if fx.rel == RelNone || fx.base != "" || fx.data {
				return nil, nil, fmt.Errorf("a64: undefined label %q", fx.label)
			}
			p.words[fx.at] = fx.enc(0) // opcode bits with a zero immediate
			relocs = append(relocs, Reloc{Offset: fx.at * 4, Sym: fx.label, Kind: fx.rel})
			continue
		}
		ref := fx.at
		if fx.base != "" {
			b, ok := p.labels[fx.base]
			if !ok {
				return nil, nil, fmt.Errorf("a64: undefined base label %q", fx.base)
			}
			ref = b
		}
		off := int32(target-ref) * 4
		if fx.data {
			p.words[fx.at] = uint32(off)
			continue
		}
		lim := int32(1) << (fx.bits - 1)
		if off < -lim || off >= lim {
			return nil, nil, fmt.Errorf("a64: %s to %q is %d bytes, out of range", fx.kind, fx.label, off)
		}
		p.words[fx.at] = fx.enc(off)
	}
	return p.words2bytes(), relocs, nil
}

// words2bytes serializes the instruction words to little-endian bytes.
func (p *Program) words2bytes() []byte {
	out := make([]byte, 4*len(p.words))
	for i, w := range p.words {
		out[4*i+0] = byte(w)
		out[4*i+1] = byte(w >> 8)
		out[4*i+2] = byte(w >> 16)
		out[4*i+3] = byte(w >> 24)
	}
	return out
}

// Labels returns the defined labels and their word indices (multiply by 4 for a
// byte offset).
func (p *Program) Labels() map[string]int { return p.labels }

// IsGlobal reports whether a label was marked global with Globl.
func (p *Program) IsGlobal(name string) bool { return p.globals[name] }
