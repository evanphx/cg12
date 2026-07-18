package lift

import (
	"fmt"

	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/ir"
)

// liftInst lowers one non-terminator instruction to load-operate-store on the
// register slots. The class handling mirrors the hardware: a w-op computes in
// ClsW (which truncates) and def zero-extends the 32-bit result into the slot.
func (l *lifter) liftInst(b *ir.Block, idx int, in *a64.Inst) error {
	c := cls(in.W64)

	switch in.Op {
	case a64.OpNop:
		return nil
	case a64.OpBl:
		return l.liftCall(b, idx)
	case a64.OpMovz:
		l.def(b, in.Rd, l.f.ConstInt(c, int64(uint64(in.Imm)<<in.ShiftAmt)), in.W64)
	case a64.OpMovn:
		l.def(b, in.Rd, l.f.ConstInt(c, int64(^(uint64(in.Imm)<<in.ShiftAmt))), in.W64)
	case a64.OpMovk:
		cur := l.src(b, in.Rd)
		mask := int64(^(uint64(0xffff) << in.ShiftAmt))
		cleared := b.And(ir.ClsL, cur, l.f.Long(mask))
		l.def(b, in.Rd, b.Or(ir.ClsL, cleared, l.f.Long(int64(uint64(in.Imm)<<in.ShiftAmt))), in.W64)
	case a64.OpMov:
		if in.HasImm {
			l.def(b, in.Rd, l.f.ConstInt(c, in.Imm), in.W64)
		} else {
			l.def(b, in.Rd, l.src(b, in.Rm), in.W64)
		}

	case a64.OpAdd, a64.OpSub:
		// add/sub-immediate is the sp form (frame adjust, mov to/from sp); the
		// register form uses the zero register for x31.
		y, err := l.operand2(b, in)
		if err != nil {
			return err
		}
		x := l.load(b, in.Rn, in.HasImm)
		l.store(b, in.Rd, l.binop(b, in.Op, c, x, y), in.W64, in.HasImm)
	case a64.OpAnd, a64.OpOrr, a64.OpEor:
		y, err := l.operand2(b, in)
		if err != nil {
			return err
		}
		l.def(b, in.Rd, l.binop(b, in.Op, c, l.src(b, in.Rn), y), in.W64)
	case a64.OpBic, a64.OpOrn, a64.OpEon:
		y, err := l.operand2(b, in)
		if err != nil {
			return err
		}
		y = b.Xor(c, y, allOnes(l.f, c)) // the inverted second operand
		op := map[a64.Mnemonic]a64.Mnemonic{a64.OpBic: a64.OpAnd, a64.OpOrn: a64.OpOrr, a64.OpEon: a64.OpEor}[in.Op]
		l.def(b, in.Rd, l.binop(b, op, c, l.src(b, in.Rn), y), in.W64)
	case a64.OpMvn:
		y, err := l.operand2(b, in)
		if err != nil {
			return err
		}
		l.def(b, in.Rd, b.Xor(c, y, allOnes(l.f, c)), in.W64)

	case a64.OpNeg:
		y, err := l.operand2(b, in)
		if err != nil {
			return err
		}
		l.def(b, in.Rd, b.Neg(c, y), in.W64)
	case a64.OpMul:
		l.def(b, in.Rd, b.Mul(c, l.src(b, in.Rn), l.src(b, in.Rm)), in.W64)
	case a64.OpMadd:
		p := b.Mul(c, l.src(b, in.Rn), l.src(b, in.Rm))
		l.def(b, in.Rd, b.Add(c, l.src(b, in.Ra), p), in.W64)
	case a64.OpMsub:
		p := b.Mul(c, l.src(b, in.Rn), l.src(b, in.Rm))
		l.def(b, in.Rd, b.Sub(c, l.src(b, in.Ra), p), in.W64)
	case a64.OpUDiv:
		l.def(b, in.Rd, b.UDiv(c, l.src(b, in.Rn), l.src(b, in.Rm)), in.W64)
	case a64.OpSDiv:
		l.def(b, in.Rd, b.Div(c, l.src(b, in.Rn), l.src(b, in.Rm)), in.W64)

	case a64.OpLslv:
		l.def(b, in.Rd, b.Shl(c, l.src(b, in.Rn), l.src(b, in.Rm)), in.W64)
	case a64.OpLsrv:
		l.def(b, in.Rd, b.Shr(c, l.src(b, in.Rn), l.src(b, in.Rm)), in.W64)
	case a64.OpAsrv:
		l.def(b, in.Rd, b.Sar(c, l.src(b, in.Rn), l.src(b, in.Rm)), in.W64)
	case a64.OpLsl:
		l.def(b, in.Rd, b.Shl(c, l.src(b, in.Rn), l.f.ConstInt(c, in.Imm)), in.W64)
	case a64.OpLsr:
		l.def(b, in.Rd, b.Shr(c, l.src(b, in.Rn), l.f.ConstInt(c, in.Imm)), in.W64)
	case a64.OpAsr:
		l.def(b, in.Rd, b.Sar(c, l.src(b, in.Rn), l.f.ConstInt(c, in.Imm)), in.W64)
	case a64.OpClz:
		l.def(b, in.Rd, b.Clz(c, l.src(b, in.Rn)), in.W64)

	case a64.OpSxtb:
		l.def(b, in.Rd, b.Extsb(c, l.src(b, in.Rn)), in.W64)
	case a64.OpSxth:
		l.def(b, in.Rd, b.Extsh(c, l.src(b, in.Rn)), in.W64)
	case a64.OpSxtw:
		l.def(b, in.Rd, b.Extsw(ir.ClsL, l.src(b, in.Rn)), true)
	case a64.OpUxtb:
		l.def(b, in.Rd, b.Extub(c, l.src(b, in.Rn)), in.W64)
	case a64.OpUxth:
		l.def(b, in.Rd, b.Extuh(c, l.src(b, in.Rn)), in.W64)

	case a64.OpCmp:
		y, err := l.operand2(b, in)
		if err != nil {
			return err
		}
		l.pending = &pendingCmp{a: l.narrow(b, l.src(b, in.Rn), in.W64), b: l.narrow(b, y, in.W64)}
	case a64.OpCset:
		if l.pending == nil {
			return fmt.Errorf("cset with no preceding compare")
		}
		pred, ok := predOf(in.Cond, l.pending.float)
		if !ok {
			return fmt.Errorf("unsupported condition %d", in.Cond)
		}
		l.def(b, in.Rd, b.Cmp(pred, ir.ClsL, l.pending.a, l.pending.b), in.W64)

	case a64.OpCsel, a64.OpCsinc, a64.OpCsinv, a64.OpCsneg:
		return l.liftCondSel(b, in)

	case a64.OpStp, a64.OpLdp:
		return l.liftPair(b, in)
	case a64.OpStr, a64.OpStrb, a64.OpStrh:
		return l.liftStore(b, in)
	case a64.OpLdr, a64.OpLdrb, a64.OpLdrh, a64.OpLdrsb, a64.OpLdrsh, a64.OpLdrsw:
		return l.liftLoad(b, in)

	// Floating point. cg12 ops are class-polymorphic, so a float add is Add(ClsD).
	case a64.OpFadd:
		l.vdef(b, in.Rd, b.Add(fcls(in.Dbl), l.vsrc(b, in.Rn, in.Dbl), l.vsrc(b, in.Rm, in.Dbl)))
	case a64.OpFsub:
		l.vdef(b, in.Rd, b.Sub(fcls(in.Dbl), l.vsrc(b, in.Rn, in.Dbl), l.vsrc(b, in.Rm, in.Dbl)))
	case a64.OpFmul:
		l.vdef(b, in.Rd, b.Mul(fcls(in.Dbl), l.vsrc(b, in.Rn, in.Dbl), l.vsrc(b, in.Rm, in.Dbl)))
	case a64.OpFdiv:
		l.vdef(b, in.Rd, b.Div(fcls(in.Dbl), l.vsrc(b, in.Rn, in.Dbl), l.vsrc(b, in.Rm, in.Dbl)))
	case a64.OpFneg:
		l.vdef(b, in.Rd, b.Neg(fcls(in.Dbl), l.vsrc(b, in.Rn, in.Dbl)))
	case a64.OpFmovReg:
		l.vdef(b, in.Rd, l.vsrc(b, in.Rn, in.Dbl))
	case a64.OpFmovVec:
		l.vdef(b, in.Rd, l.vsrc(b, in.Rn, true)) // whole-register copy
	case a64.OpFcmp:
		l.pending = &pendingCmp{a: l.vsrc(b, in.Rn, in.Dbl), b: l.vsrc(b, in.Rm, in.Dbl), float: true}
	case a64.OpFcvtStoD:
		l.vdef(b, in.Rd, b.Exts(l.vsrc(b, in.Rn, false)))
	case a64.OpFcvtDtoS:
		l.vdef(b, in.Rd, b.Truncd(l.vsrc(b, in.Rn, true)))
	case a64.OpFcvtzs:
		l.def(b, in.Rd, b.Stosi(c, l.vsrc(b, in.Rn, in.Dbl)), in.W64)
	case a64.OpFcvtzu:
		l.def(b, in.Rd, b.Stoui(c, l.vsrc(b, in.Rn, in.Dbl)), in.W64)
	case a64.OpScvtf:
		l.vdef(b, in.Rd, b.Sltof(fcls(in.Dbl), l.narrowInt(b, l.src(b, in.Rn), in.W64)))
	case a64.OpUcvtf:
		l.vdef(b, in.Rd, b.Ultof(fcls(in.Dbl), l.narrowInt(b, l.src(b, in.Rn), in.W64)))
	case a64.OpFmovFromGP:
		l.vdef(b, in.Rd, b.Cast(fcls(in.Dbl), l.narrowInt(b, l.src(b, in.Rn), in.W64)))
	case a64.OpFmovToGP:
		l.def(b, in.Rd, b.Cast(c, l.vsrc(b, in.Rn, in.Dbl)), in.W64)
	case a64.OpLdrFP:
		l.vdef(b, in.Rd, b.Load(fcls(in.Dbl), l.memAddr(b, in)))
	case a64.OpStrFP:
		b.Store(l.vsrc(b, in.Rd, in.Dbl), l.memAddr(b, in))

	default:
		return fmt.Errorf("unsupported op %s", mnemName(in.Op))
	}
	return nil
}

// liftCall lifts a bl: it reads the arguments from the AAPCS registers, emits an
// OCall to the resolved callee, and writes the result back to x0/v0.
func (l *lifter) liftCall(b *ir.Block, idx int) error {
	name, ok := l.callName[idx]
	if !ok {
		return fmt.Errorf("unresolved call at word %d", idx)
	}
	sig, ok := l.sigs[name]
	if !ok {
		return fmt.Errorf("call to unknown function %q", name)
	}

	var args []ir.Ref
	ngrn, nsrn := 0, 0
	for _, pc := range sig.params {
		if pc.IsFloat() {
			args = append(args, l.vsrc(b, a64.Reg(nsrn), pc == ir.ClsD))
			nsrn++
		} else {
			args = append(args, l.src(b, a64.Reg(ngrn)))
			ngrn++
		}
	}

	callee := l.f.Sym(name, 0)
	if sig.void {
		b.CallVoid(callee, args...)
		return nil
	}
	r := b.Call(sig.retty, callee, args...)
	if sig.retty.IsFloat() {
		l.vdef(b, 0, r)
	} else {
		l.def(b, 0, r, sig.retty != ir.ClsW)
	}
	return nil
}

// liftCondSel lifts csel and its cousins branchlessly: cg12 has no select op, so
// a mask (all-ones when the condition holds) chooses between the two inputs.
// gcc emits these where cg12 would use a branch diamond; mem2reg is not involved.
func (l *lifter) liftCondSel(b *ir.Block, in *a64.Inst) error {
	if l.pending == nil {
		return fmt.Errorf("conditional select with no preceding compare")
	}
	pred, ok := predOf(in.Cond, l.pending.float)
	if !ok {
		return fmt.Errorf("unsupported condition %d", in.Cond)
	}
	c := cls(in.W64)
	cond := b.Cmp(pred, ir.ClsL, l.pending.a, l.pending.b) // 0 or 1
	mask := b.Sub(c, l.f.ConstInt(c, 0), cond)             // all-ones if the condition holds
	rn := l.src(b, in.Rn)

	var other ir.Ref
	switch in.Op {
	case a64.OpCsel:
		other = l.src(b, in.Rm)
	case a64.OpCsinc:
		other = b.Add(c, l.src(b, in.Rm), l.f.ConstInt(c, 1))
	case a64.OpCsinv:
		other = b.Xor(c, l.src(b, in.Rm), allOnes(l.f, c))
	case a64.OpCsneg:
		other = b.Neg(c, l.src(b, in.Rm))
	}
	sel := b.Or(c, b.And(c, rn, mask), b.And(c, other, b.Xor(c, mask, allOnes(l.f, c))))
	l.def(b, in.Rd, sel, in.W64)
	return nil
}

// memAddr is a single load/store's effective address: the base (sp-context) plus
// the immediate offset.
func (l *lifter) memAddr(b *ir.Block, in *a64.Inst) ir.Ref {
	base := l.load(b, in.Rn, true)
	if in.MemOff == 0 {
		return base
	}
	return b.Add(ir.ClsL, base, l.f.Long(in.MemOff))
}

// liftPair lifts ldp/stp, including pre/post-index writeback to the base (which
// for a frame is sp).
func (l *lifter) liftPair(b *ir.Block, in *a64.Inst) error {
	base := l.load(b, in.Rn, true)
	addr := base
	if in.Mode == a64.PreIndex || (in.Mode == a64.SignedOffset && in.MemOff != 0) {
		addr = b.Add(ir.ClsL, base, l.f.Long(in.MemOff))
	}
	if in.Mode == a64.PreIndex {
		l.store(b, in.Rn, addr, true, true) // writeback before the access
	}
	sz := int64(8)
	if !in.W64 {
		sz = 4
	}
	addr2 := b.Add(ir.ClsL, addr, l.f.Long(sz))
	if in.Op == a64.OpStp {
		l.storeWidth(b, addr, l.load(b, in.Rd, false), in.W64)
		l.storeWidth(b, addr2, l.load(b, in.Rt2, false), in.W64)
	} else {
		l.store(b, in.Rd, b.Load(cls(in.W64), addr), in.W64, false)
		l.store(b, in.Rt2, b.Load(cls(in.W64), addr2), in.W64, false)
	}
	if in.Mode == a64.PostIndex {
		l.store(b, in.Rn, b.Add(ir.ClsL, base, l.f.Long(in.MemOff)), true, true)
	}
	return nil
}

func (l *lifter) liftStore(b *ir.Block, in *a64.Inst) error {
	addr := l.memAddr(b, in)
	val := l.load(b, in.Rd, false)
	switch in.Op {
	case a64.OpStr:
		l.storeWidth(b, addr, val, in.W64)
	case a64.OpStrb:
		b.StoreSub(ir.SubB, val, addr)
	case a64.OpStrh:
		b.StoreSub(ir.SubH, val, addr)
	}
	return nil
}

func (l *lifter) liftLoad(b *ir.Block, in *a64.Inst) error {
	addr := l.memAddr(b, in)
	var v ir.Ref
	switch in.Op {
	case a64.OpLdr:
		v = b.Load(cls(in.W64), addr)
	case a64.OpLdrb:
		v = b.LoadSub(cls(in.W64), ir.SubUB, addr)
	case a64.OpLdrh:
		v = b.LoadSub(cls(in.W64), ir.SubUH, addr)
	case a64.OpLdrsb:
		v = b.LoadSub(cls(in.W64), ir.SubB, addr)
	case a64.OpLdrsh:
		v = b.LoadSub(cls(in.W64), ir.SubH, addr)
	case a64.OpLdrsw:
		v = b.LoadSub(ir.ClsL, ir.SubW, addr)
	}
	l.store(b, in.Rd, v, in.W64, false)
	return nil
}

// storeWidth stores a register value at the transfer width: 8 bytes for an X
// register, 4 for a W register.
func (l *lifter) storeWidth(b *ir.Block, addr, val ir.Ref, w64 bool) {
	if !w64 {
		val = b.Copy(ir.ClsW, val)
	}
	b.Store(val, addr)
}

// fcls maps the float width to its class.
func fcls(dbl bool) ir.Cls {
	if dbl {
		return ir.ClsD
	}
	return ir.ClsS
}

// vsrc reads a v-register at the fp width. vdef writes one.
func (l *lifter) vsrc(b *ir.Block, r a64.Reg, dbl bool) ir.Ref {
	return b.Load(fcls(dbl), l.vreg[r])
}
func (l *lifter) vdef(b *ir.Block, r a64.Reg, val ir.Ref) { b.Store(val, l.vreg[r]) }

// narrowInt views a 64-bit slot value as a 32-bit integer for a w-form
// conversion, so its signedness is interpreted at the right width.
func (l *lifter) narrowInt(b *ir.Block, v ir.Ref, w64 bool) ir.Ref {
	if w64 {
		return v
	}
	return b.Copy(ir.ClsW, v)
}

// binop dispatches the plain integer binaries.
func (l *lifter) binop(b *ir.Block, op a64.Mnemonic, c ir.Cls, x, y ir.Ref) ir.Ref {
	switch op {
	case a64.OpAdd:
		return b.Add(c, x, y)
	case a64.OpSub:
		return b.Sub(c, x, y)
	case a64.OpAnd:
		return b.And(c, x, y)
	case a64.OpOrr:
		return b.Or(c, x, y)
	case a64.OpEor:
		return b.Xor(c, x, y)
	}
	panic("not a binop")
}

// narrow reduces a 64-bit slot value to the compare width, so a w-compare
// interprets signedness at 32 bits (the operand class drives the comparison).
func (l *lifter) narrow(b *ir.Block, v ir.Ref, w64 bool) ir.Ref {
	if w64 {
		return v
	}
	return b.Copy(ir.ClsW, v)
}

func allOnes(f *ir.Func, c ir.Cls) ir.Ref { return f.ConstInt(c, -1) }

func mnemName(op a64.Mnemonic) string { return fmt.Sprintf("op%d", op) }
