package bpf

import (
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// Compile lowers a single cg12 function to an eBPF program using linear-scan
// register allocation. Temporaries live in registers (r2-r9) and spill to the
// 512-byte stack only under pressure; r0 and r1 are reserved as scratch and as
// the ABI's result / first-argument registers. Values live across a helper call
// are placed in the kernel-preserved registers r6-r9.
func Compile(f *ir.Func) (*Prog, error) {
	ir.LowerPointers(f, ir.ClsL) // eBPF is 64-bit; pointers are longs
	c := &comp{f: f, allocaAt: map[*ir.Instr]int16{}, blockStart: map[*ir.Block]int{}, spillOff: map[int]int16{}}
	c.allocStack()
	c.regAlloc()
	if -c.stackTop > 512 {
		return nil, fmt.Errorf("bpf: function %s needs %d stack bytes, over the 512-byte eBPF limit", f.Name, -c.stackTop)
	}
	c.prologue()
	for _, b := range f.Blocks {
		c.blockStart[b] = len(c.insns)
		for i := range b.Instrs {
			c.instr(&b.Instrs[i])
		}
		c.term(b)
	}
	c.resolve()
	return &Prog{Insns: c.insns}, c.err
}

type comp struct {
	f          *ir.Func
	insns      []Insn
	stackTop   int16 // most-negative stack offset used (allocas + spills)
	allocaAt   map[*ir.Instr]int16
	blockStart map[*ir.Block]int
	spillOff   map[int]int16 // temp ID -> stack byte offset (negative), for spilled temps
	fixups     []fixup
	err        error
}

type fixup struct {
	at     int
	target *ir.Block
}

func (c *comp) fail(format string, a ...any) {
	if c.err == nil {
		c.err = fmt.Errorf(format, a...)
	}
}

func (c *comp) emit(in ...Insn) { c.insns = append(c.insns, in...) }

// allocStack reserves stack space for every OAlloc, below which register spills
// are placed by the allocator.
func (c *comp) allocStack() {
	for _, b := range c.f.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if !in.Op.IsAlloc() {
				continue
			}
			sz := int16(8)
			if a := in.Arg(0); a.Kind == ir.RefConst {
				sz = int16(c.f.Consts[a.ID].Int)
			}
			c.stackTop -= (sz + 7) &^ 7
			c.allocaAt[in] = c.stackTop
		}
	}
}

// prologue moves the incoming argument registers (r1-r5) into the locations the
// allocator chose for the parameters.
func (c *comp) prologue() {
	var moves []movePair
	for i, p := range c.f.Params {
		if i >= 5 {
			c.fail("bpf: function %s has more than 5 register parameters", c.f.Name)
			return
		}
		dst := c.locOf(p.Ref())
		if dst.kind == locNone {
			continue // an unused parameter needs no landing spot
		}
		moves = append(moves, movePair{dst: dst, src: regLoc(Reg(int(R1) + i))})
	}
	c.parallelMove(moves)
}

// --- locations -------------------------------------------------------------

// loc is where a value lives: a register, a stack slot, or an immediate.
type loc struct {
	reg  Reg
	slot int16
	imm  int64
	kind locKind
}

type locKind uint8

const (
	locReg locKind = iota
	locSlot
	locImm
	locNone // a dead value with no assigned location
)

func regLoc(r Reg) loc     { return loc{kind: locReg, reg: r} }
func slotLoc(o int16) loc  { return loc{kind: locSlot, slot: o} }
func immLoc(v int64) loc   { return loc{kind: locImm, imm: v} }
func (l loc) isReg() bool  { return l.kind == locReg }
func (l loc) isSlot() bool { return l.kind == locSlot }

// locOf returns where ref lives after allocation.
func (c *comp) locOf(ref ir.Ref) loc {
	switch ref.Kind {
	case ir.RefTemp:
		t := c.f.Temps[ref.ID]
		if t.Reg != ir.NoReg {
			return regLoc(Reg(t.Reg))
		}
		if off, ok := c.spillOff[int(ref.ID)]; ok {
			return slotLoc(off)
		}
		return loc{kind: locNone} // a dead temporary with no location
	case ir.RefConst:
		cst := c.f.Consts[ref.ID]
		if cst.Kind == ir.ConstInt {
			return immLoc(cst.Int)
		}
		c.fail("bpf: unsupported constant %v (symbols/floats are not supported)", cst.Kind)
	}
	return immLoc(0)
}

// ldImm loads an immediate into dst, using the wide form when it does not fit a
// signed 32-bit field.
func (c *comp) ldImm(dst Reg, v int64) {
	if v >= -0x80000000 && v <= 0x7fffffff {
		c.emit(Mov64Imm(dst, int32(v)))
		return
	}
	w := LdImm64(dst, v)
	c.emit(w[0], w[1])
}

// into materializes ref's value into register dst.
func (c *comp) into(dst Reg, ref ir.Ref) {
	l := c.locOf(ref)
	switch l.kind {
	case locReg:
		if l.reg != dst {
			c.emit(Mov64Reg(dst, l.reg))
		}
	case locSlot:
		c.emit(LdxMem(sizeDW, dst, R10, l.slot))
	case locImm:
		c.ldImm(dst, l.imm)
	}
}

// regOf returns a register holding ref: its own if allocated to one, otherwise
// scratch after loading ref into it.
func (c *comp) regOf(ref ir.Ref, scratch Reg) Reg {
	if l := c.locOf(ref); l.isReg() {
		return l.reg
	}
	c.into(scratch, ref)
	return scratch
}

// destReg returns the register to compute a result in: the temp's own register,
// or scratchA when it is spilled (then written back with spillBack).
func (c *comp) destReg(to ir.Ref) Reg {
	if l := c.locOf(to); l.isReg() {
		return l.reg
	}
	return scratchA
}

// spillBack stores a computed result to its stack slot when the result temp was
// spilled (destReg returned scratchA).
func (c *comp) spillBack(to ir.Ref, src Reg) {
	if to.IsNone() {
		return
	}
	if l := c.locOf(to); l.isSlot() {
		c.emit(StxMem(sizeDW, R10, l.slot, src))
	}
}

// --- parallel moves --------------------------------------------------------

// movePair is one value transfer performed as part of a simultaneous move set
// (parameter setup, call arguments, phi resolution).
type movePair struct{ dst, src loc }

func sameLoc(a, b loc) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case locReg:
		return a.reg == b.reg
	case locSlot:
		return a.slot == b.slot
	default:
		return a.imm == b.imm
	}
}

// readByOthers reports whether some other pending move's source still reads
// move i's destination (so i cannot be emitted yet without clobbering it).
func readByOthers(work []movePair, i int) bool {
	for j := range work {
		if j != i && sameLoc(work[j].src, work[i].dst) {
			return true
		}
	}
	return false
}

// parallelMove performs a set of transfers as if simultaneous, ordering them so
// no move clobbers a value another still needs and breaking cycles through the
// scratch register r0.
func (c *comp) parallelMove(moves []movePair) {
	var work []movePair
	for _, m := range moves {
		if !sameLoc(m.dst, m.src) {
			work = append(work, m)
		}
	}
	for len(work) > 0 {
		idx := -1
		for i := range work {
			if !readByOthers(work, i) {
				idx = i
				break
			}
		}
		if idx >= 0 {
			c.emitMove(work[idx].dst, work[idx].src)
			work = append(work[:idx], work[idx+1:]...)
			continue
		}
		// Every remaining move is part of a cycle: rescue one destination's
		// current value into r0 and let its readers take it from there.
		d := work[0].dst
		c.emitMove(regLoc(scratchA), d)
		for i := range work {
			if sameLoc(work[i].src, d) {
				work[i].src = regLoc(scratchA)
			}
		}
	}
}

// emitMove transfers one value from src to dst.
func (c *comp) emitMove(dst, src loc) {
	switch dst.kind {
	case locReg:
		switch src.kind {
		case locReg:
			if src.reg != dst.reg {
				c.emit(Mov64Reg(dst.reg, src.reg))
			}
		case locSlot:
			c.emit(LdxMem(sizeDW, dst.reg, R10, src.slot))
		case locImm:
			c.ldImm(dst.reg, src.imm)
		}
	case locSlot:
		switch src.kind {
		case locReg:
			c.emit(StxMem(sizeDW, R10, dst.slot, src.reg))
		case locSlot:
			c.emit(LdxMem(sizeDW, scratchB, R10, src.slot), StxMem(sizeDW, R10, dst.slot, scratchB))
		case locImm:
			c.ldImm(scratchB, src.imm)
			c.emit(StxMem(sizeDW, R10, dst.slot, scratchB))
		}
	}
}

// resolve patches each recorded jump with the relative offset to its target.
func (c *comp) resolve() {
	for _, fx := range c.fixups {
		target, ok := c.blockStart[fx.target]
		if !ok {
			c.fail("bpf: jump to unknown block %q", fx.target.Name)
			return
		}
		off := target - (fx.at + 1)
		if off < -0x8000 || off > 0x7fff {
			c.fail("bpf: jump offset %d out of range", off)
			return
		}
		c.insns[fx.at].Off = int16(off)
	}
}
