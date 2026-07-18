package lift

import (
	"fmt"

	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/ir"
)

// emit lays out the register slots, seeds the argument registers, and lifts each
// block's instructions and terminator.
// stackBytes is the size of the lifted stack buffer sp points into; frame saves
// and spills land here as ordinary memory.
const stackBytes = 1 << 16

func (l *lifter) emit() error {
	entry := l.blockAt[0]
	for i := 0; i < numGPR; i++ {
		l.reg[i] = entry.Alloc(8, 8)
		l.vreg[i] = entry.Alloc(8, 8)
	}
	// The stack: sp starts at the top of a buffer and grows down. The buffer is
	// addressed by computed sp-relative addresses, so mem2reg leaves it as memory
	// while the sp slot itself (only loaded/stored) promotes.
	l.spSlot = entry.Alloc(8, 8)
	stack := entry.Copy(ir.ClsL, entry.Alloc(16, stackBytes))
	entry.Store(entry.Add(ir.ClsL, stack, l.f.Long(stackBytes)), l.spSlot)

	// AAPCS: integer arguments fill x0.. and float arguments fill v0.. on separate
	// counters; the result comes back in x0 (int) or v0 (float).
	ngrn, nsrn := 0, 0
	for i, p := range l.params {
		if l.paramCls[i].IsFloat() {
			if nsrn >= 8 {
				return fmt.Errorf("more than 8 float arguments is unsupported")
			}
			entry.Store(p, l.vreg[nsrn])
			nsrn++
			continue
		}
		if ngrn >= 8 {
			return fmt.Errorf("more than 8 integer arguments is unsupported")
		}
		if l.paramCls[i] == ir.ClsW {
			p = entry.Extuw(ir.ClsL, p) // a w-arg occupies the low 32 of x_i
		}
		entry.Store(p, l.reg[ngrn])
		ngrn++
	}

	for _, start := range l.leaders {
		b := l.blockAt[start]
		l.pending = nil
		s, e := l.blockRange(start)
		term := e
		if e > s && isBlockEnd(l.insts[e-1].Op) {
			term = e - 1
		}
		for i := s; i < term; i++ {
			if err := l.liftInst(b, &l.insts[i]); err != nil {
				return fmt.Errorf("word %d (%s): %w", i, mnemName(l.insts[i].Op), err)
			}
		}
		if err := l.terminate(b, term, e); err != nil {
			return err
		}
	}
	return nil
}

// terminate sets a block's terminator: the branch/return at index term (if the
// block ends in one) or a fall-through Goto to the next block.
func (l *lifter) terminate(b *ir.Block, term, end int) error {
	if term >= end || !isBlockEnd(l.insts[term].Op) {
		b.Goto(l.blockAt[end]) // fall through
		return nil
	}
	in := &l.insts[term]
	switch in.Op {
	case a64.OpB:
		tgt, _ := branchTarget(term, in)
		b.Goto(l.blockAt[tgt])
	case a64.OpRet:
		switch {
		case l.voidRet:
			b.RetVoid()
		case l.f.Retty.IsFloat():
			b.Ret(b.Load(l.f.Retty, l.vreg[0])) // float result in v0
		default:
			b.Ret(b.Load(l.f.Retty, l.reg[0]))
		}
	case a64.OpCbz, a64.OpCbnz:
		tgt, _ := branchTarget(term, in)
		cond := b.Load(ir.ClsL, l.reg[in.Rd])
		if in.Op == a64.OpCbnz {
			b.Jnz(cond, l.blockAt[tgt], l.blockAt[end])
		} else {
			b.Jnz(cond, l.blockAt[end], l.blockAt[tgt])
		}
	case a64.OpBcond:
		if l.pending == nil {
			return fmt.Errorf("b.cond with no preceding compare")
		}
		pred, ok := predOf(in.Cond, l.pending.float)
		if !ok {
			return fmt.Errorf("unsupported condition %d", in.Cond)
		}
		tgt, _ := branchTarget(term, in)
		cond := b.Cmp(pred, ir.ClsW, l.pending.a, l.pending.b)
		b.Jnz(cond, l.blockAt[tgt], l.blockAt[end])
	default:
		return fmt.Errorf("unsupported terminator %s", mnemName(in.Op))
	}
	return nil
}

// cls maps the width flag to the operand/result class.
func cls(w64 bool) ir.Cls {
	if w64 {
		return ir.ClsL
	}
	return ir.ClsW
}

// load reads register r as a full 64-bit value. Register 31 is context-
// dependent: the stack pointer where sp is meant (add/sub-immediate, memory
// bases), the zero register everywhere else.
func (l *lifter) load(b *ir.Block, r a64.Reg, sp bool) ir.Ref {
	if r == 31 {
		if sp {
			return b.Load(ir.ClsL, l.spSlot)
		}
		return l.f.Long(0)
	}
	return b.Load(ir.ClsL, l.reg[r])
}

// store writes val into register r. A write to x31 goes to sp (when sp) or is
// discarded (the zero register). A w-result is zero-extended into the 64-bit
// slot, matching the hardware.
func (l *lifter) store(b *ir.Block, r a64.Reg, val ir.Ref, w64, sp bool) {
	if r == 31 {
		if sp {
			b.Store(val, l.spSlot)
		}
		return
	}
	if !w64 {
		val = b.Extuw(ir.ClsL, val)
	}
	b.Store(val, l.reg[r])
}

func (l *lifter) src(b *ir.Block, r a64.Reg) ir.Ref { return l.load(b, r, false) }
func (l *lifter) def(b *ir.Block, r a64.Reg, val ir.Ref, w64 bool) {
	l.store(b, r, val, w64, false)
}

// operand2 is an instruction's second operand: an immediate, or Rm optionally
// shifted.
func (l *lifter) operand2(b *ir.Block, in *a64.Inst) (ir.Ref, error) {
	if in.HasImm {
		return l.f.ConstInt(cls(in.W64), in.Imm), nil
	}
	v := l.src(b, in.Rm)
	if in.ShiftAmt != 0 {
		amt := l.f.ConstInt(cls(in.W64), int64(in.ShiftAmt))
		switch in.Shift {
		case 0:
			v = b.Shl(cls(in.W64), v, amt)
		case 1:
			v = b.Shr(cls(in.W64), v, amt)
		case 2:
			v = b.Sar(cls(in.W64), v, amt)
		default:
			return ir.R, fmt.Errorf("unsupported shifted operand")
		}
	}
	return v, nil
}

// predOf inverts the backend's condition mapping (arm64/mc.go intCondOf): a cset
// under an AArch64 condition becomes the ir.Cmp that produced it.
func predOf(c a64.Cond, float bool) (ir.Cmp, bool) {
	if float {
		return fpPredOf(c)
	}
	switch c {
	case a64.EQ:
		return ir.CmpEq, true
	case a64.NE:
		return ir.CmpNe, true
	case a64.LT:
		return ir.CmpSlt, true
	case a64.LE:
		return ir.CmpSle, true
	case a64.GT:
		return ir.CmpSgt, true
	case a64.GE:
		return ir.CmpSge, true
	case a64.CC:
		return ir.CmpUlt, true
	case a64.LS:
		return ir.CmpUle, true
	case a64.HI:
		return ir.CmpUgt, true
	case a64.CS:
		return ir.CmpUge, true
	}
	return 0, false
}

// fpPredOf is the float counterpart, inverting arm64/mc.go fpCondOf.
func fpPredOf(c a64.Cond) (ir.Cmp, bool) {
	switch c {
	case a64.EQ:
		return ir.CmpFeq, true
	case a64.NE:
		return ir.CmpFne, true
	case a64.MI:
		return ir.CmpFlt, true
	case a64.LS:
		return ir.CmpFle, true
	case a64.GT:
		return ir.CmpFgt, true
	case a64.GE:
		return ir.CmpFge, true
	case a64.VC:
		return ir.CmpFo, true
	case a64.VS:
		return ir.CmpFuo, true
	}
	return 0, false
}
