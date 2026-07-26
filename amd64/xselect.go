package amd64

import "github.com/evanphx/cg12/ir"

// xsel drives amd64 instruction selection against an xasm builder, resolving
// operands to registers through the builder's move primitive, so that choosing
// which instruction to emit stays separate from encoding it (mirroring arm64's
// sel).
type xsel struct {
	f    *ir.Func
	b    xasm
	next *ir.Block // block laid out next, for fall-through elision
}

// gpValue returns a GPR holding ref's value, loading it into scratch if it is not
// already in a register.
func (s *xsel) gpValue(ref ir.Ref, scratch Reg) Reg {
	l := s.b.refLoc(ref)
	if l.kind == locReg {
		return l.reg
	}
	s.b.move(regLoc(scratch, l.size, false), l)
	return scratch
}

// fpValue returns an XMM holding ref's value, loading it into scratch if needed.
func (s *xsel) fpValue(ref ir.Ref, scratch Reg) Reg {
	l := s.b.refLoc(ref)
	if l.kind == locReg {
		return l.reg
	}
	s.b.move(regLoc(scratch, l.size, true), l)
	return scratch
}

// fpDst returns the destination XMM for a float result and a commit closure.
func (s *xsel) fpDst(ref ir.Ref) (Reg, func()) {
	t := s.f.Temps[ref.ID]
	if t.Reg != ir.NoReg {
		return Reg(t.Reg), func() {}
	}
	size := t.Cls.Size()
	slot := t.Slot
	return fpScratch0, func() { s.b.spillStoreFP(fpScratch0, slot, size) }
}

// gpInto places ref's value into GPR d.
func (s *xsel) gpInto(d Reg, ref ir.Ref) {
	l := s.b.refLoc(ref)
	s.b.move(regLoc(d, l.size, false), l)
}

// fpInto places ref's value into XMM d.
func (s *xsel) fpInto(d Reg, ref ir.Ref) {
	l := s.b.refLoc(ref)
	s.b.move(regLoc(d, l.size, true), l)
}

// gpDst returns the destination GPR for an integer result and a commit closure
// that stores it back when the result is spilled.
func (s *xsel) gpDst(ref ir.Ref) (Reg, func()) {
	t := s.f.Temps[ref.ID]
	if t.Reg != ir.NoReg {
		return Reg(t.Reg), func() {}
	}
	size := t.Cls.Size()
	slot := t.Slot
	return gpScratch0, func() { s.b.spillStore(gpScratch0, slot, size) }
}

// selectInt handles the two-operand integer arithmetic instructions
// (add/sub/mul/and/or/xor) through the builder. It reports whether it handled the
// instruction; everything else falls back to the emitter's own logic.
func (s *xsel) selectInt(in *ir.Instr) bool {
	switch in.Op {
	case ir.OAdd, ir.OSub, ir.OMul, ir.OAnd, ir.OOr, ir.OXor:
		if in.Cls.IsFloat() {
			s.binFP(in)
		} else {
			s.binInt(in)
		}
	case ir.ONeg:
		if in.Cls.IsFloat() {
			d, commit := s.fpDst(in.To)
			s.fpInto(d, in.Arg(0))
			s.b.fnegFP(in.Cls == ir.ClsD, d)
			commit()
		} else {
			d, commit := s.gpDst(in.To)
			s.gpInto(d, in.Arg(0))
			s.b.negGP(in.Cls == ir.ClsL, d)
			commit()
		}
	case ir.OShl, ir.OShr, ir.OSar:
		s.shift(in)
	case ir.ODiv:
		if in.Cls.IsFloat() {
			s.binFP(in)
		} else {
			s.div(in, true, false)
		}
	case ir.OUDiv:
		s.div(in, false, false)
	case ir.ORem:
		s.div(in, true, true)
	case ir.OURem:
		s.div(in, false, true)
	case ir.OCmp:
		if in.Cmp.IsFloat() {
			dbl := s.f.ClassOf(in.Arg(0)) == ir.ClsD
			a := s.fpValue(in.Arg(0), fpScratch0)
			bb := s.fpValue(in.Arg(1), fpScratch1)
			d, commit := s.gpDst(in.To)
			s.b.fcmpSet(in.Cmp, dbl, d, a, bb)
			commit()
		} else {
			s.cmp(in)
		}
	case ir.OExtsb, ir.OExtub, ir.OExtsh, ir.OExtuh, ir.OExtsw, ir.OExtuw:
		w := in.Cls == ir.ClsL
		rs := s.gpValue(in.Arg(0), gpScratch1)
		d, commit := s.gpDst(in.To)
		s.b.extGP(in.Op, w, d, rs)
		commit()
	case ir.OExts, ir.OTruncd, ir.OStosi, ir.OStoui, ir.OSltof, ir.OUltof, ir.OCast:
		s.convert(in)
	case ir.OCopy:
		s.b.move(s.b.refLoc(in.To), s.b.refLoc(in.Arg(0)))
	case ir.OLoadsb, ir.OLoadub, ir.OLoadsh, ir.OLoaduh, ir.OLoadsw, ir.OLoaduw, ir.OLoadl, ir.OLoads, ir.OLoadd:
		s.load(in)
	case ir.OStoreb, ir.OStoreh, ir.OStorew, ir.OStorel, ir.OStores, ir.OStored:
		s.store(in)
	case ir.OAllocN:
		size := s.gpValue(in.Args[0], gpScratch0)
		d, commit := s.gpDst(in.To)
		s.b.allocNSP(d, size)
		commit()
	case ir.OIntrinsic:
		switch in.Intrin.Name {
		case "stacksave":
			d, commit := s.gpDst(in.To)
			s.b.movFromSP(d)
			commit()
		case "stackrestore":
			s.b.movToSP(s.gpValue(in.Args[0], gpScratch0))
		case "constant_p":
			// An unresolved __builtin_constant_p (normally settled by opt): 0 is sound.
			s.b.move(s.b.refLoc(in.To), s.b.refLoc(s.f.ConstInt(in.Cls, 0)))
		default:
			s.b.fail("amd64: unsupported intrinsic %q", in.Intrin.Name)
		}
	case ir.OGetReg:
		src := regLoc(Reg(in.Arg(0).ID), in.Cls.Size(), in.Cls.IsFloat())
		s.b.move(s.b.refLoc(in.To), src)
	case ir.OSetReg:
		dst := regLoc(Reg(in.Arg(1).ID), in.Cls.Size(), in.Cls.IsFloat())
		s.b.move(dst, s.b.refLoc(in.Arg(0)))
	case ir.OBlockAddr:
		s.b.blockAddrLea(gpScratch0, in.Blk)
		s.b.move(s.b.refLoc(in.To), regLoc(gpScratch0, 8, false))
	default:
		return false
	}
	return true
}

// term selects a block terminator through the builder, handling the branch forms
// and the jump table. It returns false only for the return terminator, which
// stays on each emitter's own path because it drives the frame epilogue.
// fusedBranch emits a trailing integer comparison (already emitted flags-only in
// the block body) as a direct conditional jump: to To when the predicate holds,
// else to To2.
func (s *xsel) fusedBranch(b *ir.Block, cmp *ir.Instr) {
	s.b.jcc(intCond(cmp.Cmp), b.Jmp.To, b.Jmp.To2, s.next)
}

func (s *xsel) term(b *ir.Block) bool {
	switch b.Jmp.Kind {
	case ir.JmpJmp:
		if b.Jmp.To != s.next { // else fall through
			s.b.jmp(b.Jmp.To)
		}
	case ir.JmpJnz:
		w := s.f.ClassOf(b.Jmp.Arg) == ir.ClsL
		s.b.jnz(s.gpValue(b.Jmp.Arg, gpScratch0), w, b.Jmp.To, b.Jmp.To2)
	case ir.JmpHlt:
		s.b.hlt()
	case ir.JmpBr:
		s.b.jmpReg(s.gpValue(b.Jmp.Arg, gpScratch0))
	case ir.JmpTable:
		s.b.jmpTable(s.gpValue(b.Jmp.Arg, gpScratch1), b, b.Jmp.Targets)
	default:
		return false
	}
	return true
}

// parallelMove performs a set of simultaneous moves, ordering them so no source
// is clobbered before it is read and breaking register cycles with a scratch. It
// is written once here, above the move primitive that emits each one.
func (s *xsel) parallelMove(pairs []locPair) {
	var work []locPair
	for _, p := range pairs {
		if !sameLoc(p.dst, p.src) {
			work = append(work, p)
		}
	}
	for len(work) > 0 {
		idx := -1
		for i, p := range work {
			blocked := false
			for j, q := range work {
				if i != j && srcReadsDst(q.src, p.dst) {
					blocked = true
					break
				}
			}
			if !blocked {
				idx = i
				break
			}
		}
		if idx >= 0 {
			s.b.move(work[idx].dst, work[idx].src)
			work = append(work[:idx], work[idx+1:]...)
			continue
		}
		// Cyclic: rescue a register destination into scratch and reroute readers.
		ci := -1
		for i, p := range work {
			if p.dst.kind == locReg {
				ci = i
				break
			}
		}
		if ci < 0 {
			s.b.fail("amd64: unexpected memory cycle in parallel move")
			return
		}
		saved := work[ci].dst
		rescue := gpScratch0
		if saved.float {
			rescue = fpScratch0
		}
		tmp := regLoc(rescue, saved.size, saved.float)
		s.b.move(tmp, saved)
		for i := range work {
			if srcReadsDst(work[i].src, saved) {
				work[i].src = tmp
			}
		}
	}
}

// teardown restores callee-saved registers and unwinds the frame (mov rsp,rbp;
// pop rbp), leaving rsp at the return address without returning. It is shared by
// the return epilogue and the tail-call branch.
func (s *xsel) teardown(lay *frameLayout) {
	for k, r := range lay.calleeSaved {
		s.b.restoreGP(r, lay.savedAddr(k))
	}
	s.b.framePop()
}

// epilogue tears down the frame and returns.
func (s *xsel) epilogue(lay *frameLayout) {
	s.teardown(lay)
	s.b.ret()
}

// call emits a direct or indirect call. Args[0] is the callee: a symbol constant
// becomes a direct call, anything else an indirect call through a register. The
// caller has already placed the arguments (emitArgs) and set the vararg count.
func (s *xsel) call(in *ir.Instr) {
	callee := in.Args[0]
	if callee.Kind == ir.RefConst {
		if c := s.f.Consts[callee.ID]; c.Kind == ir.ConstSym {
			s.b.callSym(c.Sym, c.Int)
			return
		}
	}
	s.b.callReg(s.gpValue(callee, gpScratch0))
}

// load emits a load, choosing a GP or XMM destination by the op.
func (s *xsel) load(in *ir.Instr) {
	if in.Op == ir.OLoads || in.Op == ir.OLoadd {
		d, commit := s.fpDst(in.To)
		s.b.loadFP(in.Op, d, in.Arg(0))
		commit()
		return
	}
	d, commit := s.gpDst(in.To)
	s.b.loadGP(in, in.Cls == ir.ClsL, d)
	commit()
}

// store emits a store; Args[0] is the value, Args[1] the address.
func (s *xsel) store(in *ir.Instr) {
	if in.Op == ir.OStores || in.Op == ir.OStored {
		s.b.storeFP(in.Op, s.fpValue(in.Arg(0), fpScratch0), in.Arg(1))
		return
	}
	s.b.storeGP(in, s.gpValue(in.Arg(0), gpScratch0))
}

// binFP computes dst = arg0 OP arg1 in x86's two-operand form for floats.
func (s *xsel) binFP(in *ir.Instr) {
	dbl := in.Cls == ir.ClsD
	d, commit := s.fpDst(in.To)
	rb := s.fpValue(in.Arg(1), fpScratch1)
	if rb == d {
		s.b.movFP(dbl, fpScratch1, rb)
		rb = fpScratch1
	}
	s.fpInto(d, in.Arg(0))
	s.b.binFP(in.Op, dbl, d, rb)
	commit()
}

// convert emits a float/int conversion or int<->float bitcast.
func (s *xsel) convert(in *ir.Instr) {
	switch in.Op {
	case ir.OExts:
		rs := s.fpValue(in.Arg(0), fpScratch1)
		d, commit := s.fpDst(in.To)
		s.b.cvtSS2SD(d, rs)
		commit()
	case ir.OTruncd:
		rs := s.fpValue(in.Arg(0), fpScratch1)
		d, commit := s.fpDst(in.To)
		s.b.cvtSD2SS(d, rs)
		commit()
	case ir.OStosi, ir.OStoui:
		srcD := s.f.ClassOf(in.Arg(0)) == ir.ClsD
		w := in.Cls == ir.ClsL
		rs := s.fpValue(in.Arg(0), fpScratch1)
		d, commit := s.gpDst(in.To)
		s.b.cvtF2SI(w, srcD, d, rs)
		commit()
	case ir.OSltof, ir.OUltof:
		dstD := in.Cls == ir.ClsD
		w := s.f.ClassOf(in.Arg(0)) == ir.ClsL
		rs := s.gpValue(in.Arg(0), gpScratch1)
		d, commit := s.fpDst(in.To)
		s.b.cvtSI2F(w, dstD, d, rs)
		commit()
	case ir.OCast:
		if in.Cls.IsFloat() {
			rs := s.gpValue(in.Arg(0), gpScratch1)
			d, commit := s.fpDst(in.To)
			s.b.castG2F(in.Cls == ir.ClsD, d, rs)
			commit()
		} else {
			rs := s.fpValue(in.Arg(0), fpScratch1)
			d, commit := s.gpDst(in.To)
			s.b.castF2G(in.Cls == ir.ClsL, d, rs)
			commit()
		}
	}
}

// div emits an integer divide/remainder: place the dividend in RAX, divide, then
// move the quotient (RAX) or remainder (RDX) to the destination.
func (s *xsel) div(in *ir.Instr, signed, rem bool) {
	w := in.Cls == ir.ClsL
	rb := s.gpValue(in.Arg(1), gpScratch1)
	s.gpInto(RAX, in.Arg(0))
	s.b.divGP(w, signed, rb)
	d, commit := s.gpDst(in.To)
	res := RAX
	if rem {
		res = RDX
	}
	s.b.movReg(w, d, res)
	commit()
}

// shift emits a shift by an immediate count or by %cl.
func (s *xsel) shift(in *ir.Instr) {
	w := in.Cls == ir.ClsL
	d, commit := s.gpDst(in.To)
	if c := intConstAMD(s.f, in.Arg(1)); c != nil {
		s.gpInto(d, in.Arg(0))
		s.b.shiftImmGP(in.Op, w, d, byte(c.Int&63))
		commit()
		return
	}
	s.gpInto(RCX, in.Arg(1))
	s.gpInto(d, in.Arg(0))
	s.b.shiftCLGP(in.Op, w, d)
	commit()
}

// cmp emits a compare and a movzx'd conditional-set of the boolean result.
// cmpFlags emits just the comparison, leaving the result in the flags for a
// caller that branches on them directly (see the fused compare-branch in
// mc.block). It handles the integer forms; a float compare is not fused.
func (s *xsel) cmpFlags(in *ir.Instr) {
	argW := s.f.ClassOf(in.Arg(0)) == ir.ClsL
	ra := s.gpValue(in.Arg(0), gpScratch0)
	if lb := s.b.refLoc(in.Arg(1)); lb.kind == locMem {
		s.b.cmpGPMem(argW, ra, lb.base, lb.off)
		return
	}
	rb := s.gpValue(in.Arg(1), gpScratch1)
	s.b.cmpGP(argW, ra, rb)
}

func (s *xsel) cmp(in *ir.Instr) {
	s.cmpFlags(in)
	d, commit := s.gpDst(in.To)
	s.b.setccMovzx(in.Cmp, d)
	commit()
}

// intConstAMD returns ref's integer constant, or nil.
func intConstAMD(f *ir.Func, ref ir.Ref) *ir.Const {
	if ref.Kind == ir.RefConst {
		if c := f.Consts[ref.ID]; c.Kind == ir.ConstInt {
			return &c
		}
	}
	return nil
}

// binInt computes dst = arg0 OP arg1 in x86's two-operand form: place arg0 in the
// destination, then apply the op with arg1. If arg1 already occupies the
// destination register it is moved aside first.
func (s *xsel) binInt(in *ir.Instr) {
	w := in.Cls == ir.ClsL
	d, commit := s.gpDst(in.To)
	// If the second operand is spilled, read it straight from its slot as a memory
	// operand rather than loading it into a scratch register first. The base is the
	// frame pointer, which gpInto (writing d) never disturbs, so ordering is free.
	if lb := s.b.refLoc(in.Arg(1)); lb.kind == locMem {
		s.gpInto(d, in.Arg(0))
		s.b.binGPMem(in.Op, w, d, lb.base, lb.off)
		commit()
		return
	}
	rb := s.gpValue(in.Arg(1), gpScratch1)
	if rb == d {
		s.b.movReg(w, gpScratch1, rb)
		rb = gpScratch1
	}
	s.gpInto(d, in.Arg(0))
	s.b.binGP(in.Op, w, d, rb)
	commit()
}
