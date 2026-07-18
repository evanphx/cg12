package arm64

import (
	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/ir"
)

// sel drives instruction selection against an asmb builder. It resolves each
// operand to a physical register -- loading a spilled temporary or materializing
// a constant through the builder -- so that choosing which instruction to emit
// stays separate from encoding it.
type sel struct {
	f         *ir.Func
	b         asmb
	spillBase int
}

// src resolves a source operand to a register, loading a spilled temporary into
// scratch register slot `slot` or materializing a constant there. It is
// class-aware: a floating operand uses the FP scratch registers and FP loads.
func (s *sel) src(ref ir.Ref, slot, size int) Reg {
	float := s.f.ClassOf(ref).IsFloat()
	scr := intScratchRegs[slot]
	if float {
		scr = floatScratchRegs[slot]
	}
	switch ref.Kind {
	case ir.RefTemp:
		t := s.f.Temps[ref.ID]
		if t.Reg != ir.NoReg {
			return Reg(t.Reg)
		}
		s.b.ldrSpill(scr, float, s.spillBase+t.Slot, size)
		return scr
	case ir.RefConst:
		c := s.f.Consts[ref.ID]
		if float {
			if bits, ok := floatConstBits(c); ok {
				gp := intScratchRegs[0]
				s.b.movImm(gp, bits, size == 8)
				s.b.fmovFromGP(size == 8, scr, gp)
				return scr
			}
		} else if c.Kind == ir.ConstInt {
			s.b.movImm(scr, c.Int, size == 8)
			return scr
		} else if c.Kind == ir.ConstSym {
			s.b.materializeSym(scr, c)
			return scr
		}
	}
	s.b.fail("arm64: cannot materialize operand %v", ref)
	return scr
}

// dst resolves a destination operand to a register plus a finalizer that stores a
// spilled result back. A spilled result uses scratch slot 0, which a single
// three-operand instruction may share with its first source (read before write).
func (s *sel) dst(ref ir.Ref, size int) (Reg, func()) {
	t := s.f.Temps[ref.ID]
	if t.Reg != ir.NoReg {
		return Reg(t.Reg), func() {}
	}
	float := t.Cls.IsFloat()
	scr := intScratchRegs[0]
	if float {
		scr = floatScratchRegs[0]
	}
	off := s.spillBase + t.Slot
	return scr, func() { s.b.strSpill(scr, float, off, size) }
}

// selectInt handles the integer data-processing instructions through the builder.
// It reports whether it handled the instruction; float and other ops fall back to
// the emitter's own logic during the migration.
func (s *sel) selectData(in *ir.Instr) bool {
	sz := in.Cls.Size()
	w64 := sz == 8
	flt := in.Cls.IsFloat()
	switch in.Op {
	case ir.OThreadPtr:
		d, done := s.dst(in.To, 8)
		s.b.threadPtr(d)
		done()
	case ir.OTLSIndexAddr:
		c, ok := threadConst(s.f, in.Args[0])
		if !ok {
			s.b.fail("arm64: tlsindexaddr needs a thread-local symbol, got %v", in.Args[0])
			return true
		}
		d, done := s.dst(in.To, 8)
		s.b.tlsIndexAddr(d, c)
		done()
	case ir.OTLSOffset:
		c, ok := threadConst(s.f, in.Args[0])
		if !ok {
			s.b.fail("arm64: tlsoffset needs a thread-local symbol, got %v", in.Args[0])
			return true
		}
		d, done := s.dst(in.To, 8)
		s.b.tlsOffset(d, c)
		done()
	case ir.OAdd:
		if flt {
			s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.fop(fAdd, w64, rd, rn, rm) })
		} else {
			s.addSub(in, false)
		}
	case ir.OSub:
		if flt {
			s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.fop(fSub, w64, rd, rn, rm) })
		} else {
			s.addSub(in, true)
		}
	case ir.OMul:
		if flt {
			s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.fop(fMul, w64, rd, rn, rm) })
		} else {
			s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.mul(w64, rd, rn, rm) })
		}
	case ir.ODiv:
		if flt {
			s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.fop(fDiv, w64, rd, rn, rm) })
		} else {
			s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.sdiv(w64, rd, rn, rm) })
		}
	case ir.OUDiv:
		s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.udiv(w64, rd, rn, rm) })
	case ir.ORem:
		s.rem(in, sz, true)
	case ir.OURem:
		s.rem(in, sz, false)
	case ir.OAnd:
		s.logical(in, logAnd)
	case ir.OOr:
		s.logical(in, logOrr)
	case ir.OXor:
		s.logical(in, logEor)
	case ir.OBic:
		s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.logicalReg(logBic, w64, rd, rn, rm) })
	case ir.OShl:
		s.shift(in, shLsl)
	case ir.OShr:
		s.shift(in, shLsr)
	case ir.OSar:
		s.shift(in, shAsr)
	case ir.ORotr:
		s.rotr(in)
	case ir.ONeg:
		d, done := s.dst(in.To, sz)
		rn := s.src(in.Args[0], 1, sz)
		if flt {
			s.b.fneg(w64, d, rn)
		} else {
			s.b.neg(w64, d, rn)
		}
		done()
	case ir.OClz:
		d, done := s.dst(in.To, sz)
		s.b.clz(w64, d, s.src(in.Args[0], 1, sz))
		done()
	case ir.OCmp:
		s.cmp(in)
	case ir.OSel:
		s.sel(in)

	// Conversions.
	case ir.OExts:
		s.conv1(in, func(rd, rn Reg) { s.b.fcvtStoD(rd, rn) })
	case ir.OTruncd:
		s.conv1(in, func(rd, rn Reg) { s.b.fcvtDtoS(rd, rn) })
	case ir.OStosi:
		s.conv1(in, func(rd, rn Reg) { s.b.fcvtzs(w64, s.srcSize(in) == 8, rd, rn) })
	case ir.OStoui:
		s.conv1(in, func(rd, rn Reg) { s.b.fcvtzu(w64, s.srcSize(in) == 8, rd, rn) })
	case ir.OSltof:
		s.conv1(in, func(rd, rn Reg) { s.b.scvtf(w64, s.srcSize(in) == 8, rd, rn) })
	case ir.OUltof:
		s.conv1(in, func(rd, rn Reg) { s.b.ucvtf(w64, s.srcSize(in) == 8, rd, rn) })
	case ir.OCast:
		s.conv1(in, func(rd, rn Reg) {
			if flt {
				s.b.fmovFromGP(w64, rd, rn)
			} else {
				s.b.fmovToGP(s.srcSize(in) == 8, rd, rn)
			}
		})

	// Integer sub-word extends.
	case ir.OExtsb:
		s.extend(in, extSb)
	case ir.OExtub:
		s.extend(in, extUb)
	case ir.OExtsh:
		s.extend(in, extSh)
	case ir.OExtuh:
		s.extend(in, extUh)
	case ir.OExtsw:
		s.extend(in, extSw)
	case ir.OExtuw:
		// A 32-bit mov zero-extends into the X register.
		d, done := s.dst(in.To, 4)
		s.b.movReg(false, d, s.src(in.Args[0], 1, 4))
		done()

	// Stack pointer and block addresses (OAlloc4/8/16 stay in the emitter: they need
	// the frame-layout offset).
	case ir.OAllocN:
		size := s.src(in.Args[0], 1, 8)
		d, done := s.dst(in.To, 8)
		s.b.allocN(d, size)
		done()
	case ir.OStackSave:
		d, done := s.dst(in.To, 8)
		s.b.movFromSP(d)
		done()
	case ir.OStackRestore:
		s.b.movToSP(s.src(in.Args[0], 0, 8))
	case ir.OBlockAddr:
		d, done := s.dst(in.To, 8)
		s.b.adr(d, in.Blk)
		done()

	default:
		switch {
		case in.Op.IsLoad():
			s.load(in)
		case in.Op.IsStore():
			s.store(in)
		default:
			return false
		}
	}
	return true
}

// load emits a load, [base] or indexed [base, index] with the extend/scale in Aux.
func (s *sel) load(in *ir.Instr) {
	sz := loadSize(in.Op, in.Cls)
	if len(in.Args) == 2 { // indexed
		base := s.src(in.Args[0], 1, 8)
		option, _ := decodeAmode(in.Amode)
		index := s.src(in.Args[1], 0, indexSize(option))
		d, done := s.dst(in.To, sz)
		s.b.loadIdx(in.Op, in.Cls, d, base, index, in.Amode)
		done()
		return
	}
	addr := s.src(in.Args[0], 1, 8)
	d, done := s.dst(in.To, sz)
	s.b.load(in.Op, in.Cls, d, addr)
	done()
}

// store emits a store, [base] or indexed [base, index].
func (s *sel) store(in *ir.Instr) {
	val := s.src(in.Args[0], 0, storeSize(in.Op))
	if len(in.Args) == 3 { // indexed
		base := s.src(in.Args[1], 1, 8)
		option, _ := decodeAmode(in.Amode)
		index := s.src(in.Args[2], 2, indexSize(option))
		s.b.storeIdx(in.Op, val, base, index, in.Amode)
		return
	}
	s.b.store(in.Op, val, s.src(in.Args[1], 1, 8))
}

// cmp emits a comparison and a conditional-set of the boolean result, folding a
// small constant into a cmp immediate.
func (s *sel) cmp(in *ir.Instr) {
	argCls := s.f.ClassOf(in.Args[0])
	sz := argCls.Size()
	w64 := sz == 8
	float := argCls.IsFloat()
	s1 := s.src(in.Args[0], 0, sz)
	switch {
	case float:
		s.b.fcmp(w64, s1, s.src(in.Args[1], 1, sz))
	default:
		if v, ok := intConst(s.f, in.Args[1]); ok {
			if imm, lsl12, flip, iok := addSubImm(v); iok && !lsl12 && !flip {
				s.b.cmpImm(w64, s1, imm)
				break
			}
		}
		s.b.cmpReg(w64, s1, s.src(in.Args[1], 1, sz))
	}
	d, done := s.dst(in.To, in.Cls.Size())
	s.b.cset(d, in.Cmp, float)
	done()
}

// sel emits a conditional select (csel / fcsel): its boolean condition is turned
// back into flags with cmp #0, then csel picks between the two arms. cond is an
// integer (int scratch), so its scratch bank is distinct from a/b's when those
// are floats.
func (s *sel) sel(in *ir.Instr) {
	sz := in.Cls.Size()
	w64 := sz == 8
	float := in.Cls.IsFloat()

	condCls := s.f.ClassOf(in.Args[0])
	cond := s.src(in.Args[0], 0, condCls.Size())
	s.b.cmpImm(condCls.Size() == 8, cond, 0)

	var a, b Reg
	if float {
		a = s.src(in.Args[1], 0, sz)
		b = s.src(in.Args[2], 1, sz)
	} else {
		a = s.src(in.Args[1], 1, sz)
		b = s.src(in.Args[2], 2, sz)
	}
	d, done := s.dst(in.To, sz)
	if float {
		s.b.fcsel(w64, d, a, b, a64.NE)
	} else {
		s.b.csel(w64, d, a, b, a64.NE)
	}
	done()
}

// parallelMove performs a set of simultaneous moves, ordering them so no source
// is clobbered before it is read and breaking register cycles with a scratch.
// The ordering is the difficult part and lives here, above the moveLoc that
// actually emits each one.
func (s *sel) parallelMove(pairs []movePairLoc) {
	var work []movePairLoc
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
			s.b.moveLoc(work[idx].dst, work[idx].src)
			work = append(work[:idx], work[idx+1:]...)
			continue
		}
		// Cyclic: rescue a register destination into scratch and reroute readers.
		ci := -1
		for i, p := range work {
			if !p.dst.mem {
				ci = i
				break
			}
		}
		if ci < 0 {
			s.b.fail("arm64: unexpected memory cycle in parallel move")
			return
		}
		saved := work[ci].dst
		rescue := scratch2
		if saved.reg.IsFloat() {
			rescue = fscratch0
		}
		tmp := loc{reg: rescue, size: saved.size}
		s.b.moveLoc(tmp, saved)
		for i := range work {
			if srcReadsDst(work[i].src, saved) && !work[i].src.mem {
				work[i].src = tmp
			}
		}
	}
}

// frameTeardown restores callee-saved registers and the frame pointer and pops
// the frame, leaving x30 (lr) at the caller's return address but not returning.
// It is shared by the return epilogue and the tail-call branch. Callee-saved
// slots are addressed from x29, which equals sp here (after any VLA growth is
// undone), so the reload is frame-base-relative.
func (s *sel) frameTeardown(frame int, hasDynAlloc bool, calleeSaved []Reg) {
	if hasDynAlloc {
		s.b.movSPFromFP() // undo any VLA growth before the frame-relative reloads
	}
	for i, r := range calleeSaved {
		s.b.ldrSpill(r, r.IsFloat(), 16+i*8, 8)
	}
	s.b.frameClose(frame)
}

// epilogue tears down the frame and returns.
func (s *sel) epilogue(frame int, hasDynAlloc bool, calleeSaved []Reg) {
	s.frameTeardown(frame, hasDynAlloc, calleeSaved)
	s.b.ret()
}

// call emits a direct or indirect call. Args[0] is the callee: a symbol constant
// becomes a direct bl, anything else (a temp, or a function-pointer literal the
// optimizer folded to a constant) is materialized and called indirectly with blr.
// The machine-code emitter records the GC safepoint after the branch itself.
func (s *sel) call(in *ir.Instr) {
	callee := in.Args[0]
	if callee.Kind == ir.RefConst {
		if c := s.f.Consts[callee.ID]; c.Kind == ir.ConstSym {
			s.b.callSym(c.Sym)
			return
		}
	}
	s.b.callReg(s.src(callee, 0, 8))
}

// term selects a block terminator through the builder, handling the branch forms
// and the jump table. It returns false only for the return terminator, which
// stays on each emitter's own path because it drives the frame epilogue.
func (s *sel) term(b *ir.Block) bool {
	switch b.Jmp.Kind {
	case ir.JmpJmp:
		s.b.branch(b.Jmp.To)
	case ir.JmpJnz:
		sz := s.f.ClassOf(b.Jmp.Arg).Size()
		r := s.src(b.Jmp.Arg, 0, sz)
		s.b.cbnz(sz == 8, r, b.Jmp.To)
		s.b.branch(b.Jmp.To2)
	case ir.JmpHlt:
		s.b.brk()
	case ir.JmpBr:
		s.b.brind(s.src(b.Jmp.Arg, 0, 8))
	case ir.JmpTable:
		s.b.jumpTable(s.src(b.Jmp.Arg, 0, 4), b, b.Jmp.Targets)
	default:
		return false
	}
	return true
}

// srcSize is the byte width of the first operand's class.
func (s *sel) srcSize(in *ir.Instr) int { return s.f.ClassOf(in.Args[0]).Size() }

// conv1 emits a one-source, one-destination conversion, resolving the source at
// its own width and the destination at the result width.
func (s *sel) conv1(in *ir.Instr, emit func(rd, rn Reg)) {
	rn := s.src(in.Args[0], 1, s.srcSize(in))
	rd, done := s.dst(in.To, in.Cls.Size())
	emit(rd, rn)
	done()
}

// extend emits an integer sub-word sign/zero extend.
func (s *sel) extend(in *ir.Instr, op extOp) {
	srcSz := s.srcSize(in)
	rn := s.src(in.Args[0], 1, srcSz)
	rd, done := s.dst(in.To, in.Cls.Size())
	s.b.ext(op, rd, rn, in.Cls.Size(), srcSz)
	done()
}

// shared integer path does not yet materialize (those instructions stay on the
// emitter's own path during the migration).
// binReg emits a three-operand register instruction via the given builder call.
func (s *sel) binReg(in *ir.Instr, sz int, emit func(rd, rn, rm Reg)) {
	s1 := s.src(in.Args[0], 0, sz)
	s2 := s.src(in.Args[1], 1, sz)
	d, done := s.dst(in.To, sz)
	emit(d, s1, s2)
	done()
}

// addSub emits an add/sub, folding a constant operand into a 12-bit immediate and
// flipping add<->sub for a negative constant.
func (s *sel) addSub(in *ir.Instr, sub bool) {
	sz := in.Cls.Size()
	w64 := sz == 8
	aRef, bRef := in.Args[0], in.Args[1]
	if !sub {
		if _, ok := intConst(s.f, bRef); !ok {
			if _, ok := intConst(s.f, aRef); ok {
				aRef, bRef = bRef, aRef
			}
		}
	}
	if v, ok := intConst(s.f, bRef); ok {
		if imm, lsl12, flip, ok := addSubImm(v); ok {
			s1 := s.src(aRef, 0, sz)
			d, done := s.dst(in.To, sz)
			if sub != flip {
				s.b.subImm(w64, d, s1, imm, lsl12)
			} else {
				s.b.addImm(w64, d, s1, imm, lsl12)
			}
			done()
			return
		}
	}
	s.binReg(in, sz, func(rd, rn, rm Reg) {
		if sub {
			s.b.subReg(w64, rd, rn, rm)
		} else {
			s.b.addReg(w64, rd, rn, rm)
		}
	})
}

// logical emits a bitwise op, folding a constant operand into a logical (bitmask)
// immediate when it encodes as one.
func (s *sel) logical(in *ir.Instr, op logicalOp) {
	sz := in.Cls.Size()
	w64 := sz == 8
	aRef, bRef := in.Args[0], in.Args[1]
	if _, ok := intConst(s.f, bRef); !ok {
		if _, ok := intConst(s.f, aRef); ok {
			aRef, bRef = bRef, aRef
		}
	}
	if v, ok := intConst(s.f, bRef); ok {
		val := uint64(v)
		if sz == 4 {
			val &= 0xffffffff
		}
		if _, _, _, ok := a64.EncodeBitmask(val, sz); ok {
			s1 := s.src(aRef, 0, sz)
			d, done := s.dst(in.To, sz)
			s.b.logicalImm(op, w64, d, s1, val)
			done()
			return
		}
	}
	s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.logicalReg(op, w64, rd, rn, rm) })
}

// shift emits a shift, using the immediate form for a constant amount.
func (s *sel) shift(in *ir.Instr, op shiftOp) {
	sz := in.Cls.Size()
	w64 := sz == 8
	if v, ok := intConst(s.f, in.Args[1]); ok {
		if sh, ok := shiftImm(v, sz); ok {
			s1 := s.src(in.Args[0], 0, sz)
			d, done := s.dst(in.To, sz)
			s.b.shiftImm(op, w64, d, s1, sh)
			done()
			return
		}
	}
	s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.shiftReg(op, w64, rd, rn, rm) })
}

// rotr emits a right rotate by a constant amount.
func (s *sel) rotr(in *ir.Instr) {
	sz := in.Cls.Size()
	w64 := sz == 8
	if v, ok := intConst(s.f, in.Args[1]); ok {
		s1 := s.src(in.Args[0], 0, sz)
		d, done := s.dst(in.To, sz)
		s.b.rotrImm(w64, d, s1, uint32(v)&uint32(sz*8-1))
		done()
		return
	}
	s.b.fail("arm64: rotate by a non-constant is not supported")
}

// rem computes a remainder: q = a / b, then r = a - q*b via msub.
func (s *sel) rem(in *ir.Instr, sz int, signed bool) {
	w64 := sz == 8
	a := s.src(in.Args[0], 0, sz)
	b := s.src(in.Args[1], 1, sz)
	q := intScratchRegs[2]
	if signed {
		s.b.sdiv(w64, q, a, b)
	} else {
		s.b.udiv(w64, q, a, b)
	}
	d, done := s.dst(in.To, sz)
	s.b.msub(w64, d, q, b, a) // d = a - q*b
	done()
}
