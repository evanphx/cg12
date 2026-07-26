package arm64

import (
	"strings"

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
	remat     map[int]rematRule // temps recomputed at each use (no spill slot)
	next      *ir.Block         // the block laid out next, for fall-through elision
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
		if rule, ok := s.remat[int(ref.ID)]; ok {
			s.b.rematerialize(scr, rule, size)
			return scr
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
	case ir.OUMulh:
		s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.umulh(rd, rn, rm) })
	case ir.OSMulh:
		s.binReg(in, sz, func(rd, rn, rm Reg) { s.b.smulh(rd, rn, rm) })
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
	case ir.OMAdd:
		s.triReg(in, sz, func(rd, rn, rm, ra Reg) { s.b.madd(w64, rd, rn, rm, ra) })
	case ir.OMSub:
		s.triReg(in, sz, func(rd, rn, rm, ra Reg) { s.b.msub(w64, rd, rn, rm, ra) })
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
	case ir.OCCmp:
		// Normally consumed by the block's branch (see fuseCcmp/ccmpFlags). If an
		// instruction sits between it and the terminator so the branch fusion does
		// not claim it, materialize the boolean: cmp; ccmp; cset on the predicate.
		s.ccmpFlags(in)
		d, done := s.dst(in.To, in.Cls.Size())
		s.b.cset(d, in.Cmp, false)
		done()
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
	case ir.OIntrinsic:
		switch {
		case in.Intrin.Name == "stacksave":
			d, done := s.dst(in.To, 8)
			s.b.movFromSP(d)
			done()
		case in.Intrin.Name == "stackrestore":
			s.b.movToSP(s.src(in.Args[0], 0, 8))
		case strings.HasPrefix(in.Intrin.Name, "atomic."):
			s.atomic(in)
		case in.Intrin.Name == "constant_p":
			// An unresolved __builtin_constant_p (normally settled by opt): 0 is sound.
			d, done := s.dst(in.To, in.Cls.Size())
			s.b.movImm(d, 0, in.Cls.Size() == 8)
			done()
		default:
			s.b.fail("arm64: unsupported intrinsic %q", in.Intrin.Name)
		}
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
	s.b.load(in.Op, in.Cls, d, addr, uint32(in.Aux))
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
	s.b.store(in.Op, val, s.src(in.Args[1], 1, 8), uint32(in.Aux))
}

// cmpFlags emits the comparison of in's two operands, setting the condition
// flags, folding a small constant into a cmp immediate. It reports whether the
// comparison was floating-point (which selects the FP condition mapping). It does
// not materialize the boolean result, so a caller that branches directly on the
// flags (see the fused compare-branch in mc.block) can skip the cset.
func (s *sel) cmpFlags(in *ir.Instr) bool {
	return s.cmpFlagsPair(in.Args[0], in.Args[1])
}

// cmpFlagsPair is cmpFlags over an explicit operand pair, so the fused
// compare-select (sel) can re-emit the comparison from a compare it absorbed.
// It uses scratch slots 0 and 1.
func (s *sel) cmpFlagsPair(aRef, bRef ir.Ref) bool {
	argCls := s.f.ClassOf(aRef)
	sz := argCls.Size()
	w64 := sz == 8
	float := argCls.IsFloat()
	s1 := s.src(aRef, 0, sz)
	switch {
	case float:
		s.b.fcmp(w64, s1, s.src(bRef, 1, sz))
	default:
		if v, ok := intConst(s.f, bRef); ok {
			if imm, lsl12, flip, iok := addSubImm(v); iok && !lsl12 && !flip {
				s.b.cmpImm(w64, s1, imm)
				return float
			}
		}
		s.b.cmpReg(w64, s1, s.src(bRef, 1, sz))
	}
	return float
}

// cmp emits a comparison and a conditional-set of the boolean result.
func (s *sel) cmp(in *ir.Instr) {
	float := s.cmpFlags(in)
	d, done := s.dst(in.To, in.Cls.Size())
	s.b.cset(d, in.Cmp, float)
	done()
}

// Fused-select modes, carried in a fused OSel's Aux to say how its condition's
// flags are produced. A fused OSel has 4 operands -- the compare's two operands
// then the two arms -- with the predicate in Cmp (see fuseCmpSel).
const (
	selFuseCmp = 1 // flags from `cmp a, b`; Cmp is the (int or float) predicate
	selFuseTst = 2 // flags from `tst a, b`; Cmp is CmpNe
)

// sel emits a conditional select (csel / fcsel). A fused select (4 operands, see
// fuseCmpSel) emits its absorbed comparison directly ahead of the csel; an
// unfused one has an already-materialized boolean, turned back into flags with
// cmp #0 and selected on NE. cond is an integer (int scratch), so its scratch
// bank is distinct from a/b's when those are floats.
func (s *sel) sel(in *ir.Instr) {
	if len(in.Args) == 4 {
		s.selFused(in)
		return
	}

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

// selFused emits a compare-select whose comparison was absorbed by fuseCmpSel:
// `cmp a, b ; csel d, t, f, <cond>` (or tst / fcsel). The comparison writes only
// NZCV, and every operand materialization below is flags-preserving (spill
// loads, movz/movk, adrp+add, fmov -- none touch the flags), so the flags set
// here survive to the csel. The comparison uses scratch slots 0 and 1, dead by
// the time the arms load into slots 1 and 2, so the destination (slot 0) and the
// two arms occupy distinct registers.
func (s *sel) selFused(in *ir.Instr) {
	sz := in.Cls.Size()
	w64 := sz == 8
	float := in.Cls.IsFloat()

	var cmpFloat bool
	if in.Aux == selFuseTst {
		s.tstFlagsPair(in.Args[0], in.Args[1])
	} else {
		cmpFloat = s.cmpFlagsPair(in.Args[0], in.Args[1])
	}
	cond, _ := condOf(in.Cmp, cmpFloat) // fuseCmpSel verified the predicate maps

	var a, b Reg
	if float {
		a = s.src(in.Args[2], 0, sz)
		b = s.src(in.Args[3], 1, sz)
	} else {
		a = s.selArm(in.Args[2], 1, sz)
		b = s.selArm(in.Args[3], 2, sz)
	}
	d, done := s.dst(in.To, sz)
	if float {
		s.b.fcsel(w64, d, a, b, cond)
	} else {
		s.b.csel(w64, d, a, b, cond)
	}
	done()
}

// selArm resolves an integer select arm, using the zero register for a constant
// 0 so `c ? a : 0` needs no materializing mov.
func (s *sel) selArm(ref ir.Ref, slot, sz int) Reg {
	if v, ok := intConst(s.f, ref); ok && v == 0 {
		return ZR
	}
	return s.src(ref, slot, sz)
}

// atomic lowers an atomic.* intrinsic. A plain load and store are a single
// acquire-load and release-store; a fence is a data memory barrier; every other
// operation (exchange, compare-and-swap, and the fetch-and-op family) is a
// load-exclusive / store-exclusive retry loop that repeats until the store
// succeeds. Each read-modify-write yields the previous value.
func (s *sel) atomic(in *ir.Instr) {
	base, bytes := atomicParts(in.Intrin.Name)
	// bytes is the memory access width; a sub-word access still uses a W register
	// and computes in W, so the register width (w64/regSize) is separate.
	w64 := bytes == 8
	regSize := 4
	if w64 {
		regSize = 8
	}

	switch base {
	case "fence":
		s.b.dmb(a64.BarrierISH)
		return
	case "load":
		addr := s.src(in.Args[0], 0, 8)
		d, done := s.dst(in.To, regSize)
		s.b.ldar(bytes, d, addr)
		done()
		return
	case "store":
		addr := s.src(in.Args[0], 0, 8)
		val := s.src(in.Args[1], 1, regSize)
		s.b.stlr(bytes, val, addr)
		return
	}

	pressure := func() {
		s.b.fail("arm64: atomic %q: not enough scratch registers under this register pressure", in.Intrin.Name)
	}

	addr := s.src(in.Args[0], 0, 8)
	old, done := s.dst(in.To, regSize) // the result is always the previous value
	if old == addr {
		// A spilled result and a spilled/constant address both claimed scratch 0;
		// the loop needs them distinct (it re-reads the address every iteration).
		pressure()
		return
	}
	retry := s.b.freshLabel("atomic_retry")

	switch base {
	case "xchg":
		val := s.src(in.Args[1], 1, regSize)
		status, ok := freeScratch(addr, val, old)
		if !ok {
			pressure()
			return
		}
		s.b.label(retry)
		s.b.ldaxr(bytes, old, addr)
		s.b.stlxr(bytes, status, val, addr)
		s.b.cbnzTo(false, status, retry)

	case "add", "sub", "and", "or", "xor":
		val := s.src(in.Args[1], 1, regSize)
		newv, ok := freeScratch(addr, val, old)
		if !ok {
			pressure()
			return
		}
		// The store-status register can reuse val's register once the new value is
		// computed -- but only when val sits in a scratch register (a constant or a
		// reloaded spill), never an allocated temporary that stays live afterward.
		status := val
		if !isScratchReg(val) {
			if status, ok = freeScratch(addr, val, old, newv); !ok {
				pressure()
				return
			}
		}
		s.b.label(retry)
		s.b.ldaxr(bytes, old, addr)
		s.atomicCompute(base, w64, newv, old, val)
		s.b.stlxr(bytes, status, newv, addr)
		s.b.cbnzTo(false, status, retry)

	case "cas":
		exp := s.src(in.Args[1], 1, regSize)
		newv := s.src(in.Args[2], 2, regSize)
		status, ok := freeScratch(addr, exp, newv, old)
		if !ok {
			pressure()
			return
		}
		out := s.b.freshLabel("atomic_cas_out")
		s.b.label(retry)
		s.b.ldaxr(bytes, old, addr)
		s.b.cmpReg(w64, old, exp)
		s.b.bcondTo(a64.NE, out) // mismatch: leave old in place, do not store
		s.b.stlxr(bytes, status, newv, addr)
		s.b.cbnzTo(false, status, retry)
		s.b.label(out)

	default:
		pressure()
		return
	}
	done()
}

// atomicCompute emits the fetch-and-op step new = op(old, val).
func (s *sel) atomicCompute(op string, w64 bool, dst, old, val Reg) {
	switch op {
	case "add":
		s.b.addReg(w64, dst, old, val)
	case "sub":
		s.b.subReg(w64, dst, old, val)
	case "and":
		s.b.logicalReg(logAnd, w64, dst, old, val)
	case "or":
		s.b.logicalReg(logOrr, w64, dst, old, val)
	case "xor":
		s.b.logicalReg(logEor, w64, dst, old, val)
	}
}

// atomicParts splits an atomic intrinsic name into its base operation and its
// access width in bytes: "atomic.add.l" -> ("add", 8), "atomic.and.b" -> ("and",
// 1). A bare "atomic.fence" has no width suffix and reports 0.
func atomicParts(name string) (base string, bytes int) {
	s := strings.TrimPrefix(name, "atomic.")
	switch {
	case strings.HasSuffix(s, ".b"):
		return strings.TrimSuffix(s, ".b"), 1
	case strings.HasSuffix(s, ".h"):
		return strings.TrimSuffix(s, ".h"), 2
	case strings.HasSuffix(s, ".w"):
		return strings.TrimSuffix(s, ".w"), 4
	case strings.HasSuffix(s, ".l"):
		return strings.TrimSuffix(s, ".l"), 8
	}
	return s, 0
}

// atomicHoldsResult reports whether an atomic intrinsic expands to an LDAXR/STLXR
// retry loop that keeps its result (the previously-loaded value) live while it still
// reads its address and value operands. For those the result register must differ
// from every operand register, or the loop would clobber an operand it re-reads --
// so register allocation must make the result interfere with its operands. The
// single-instruction forms (load/store/fence) have no such constraint.
func atomicHoldsResult(in *ir.Instr) bool {
	if in.Op != ir.OIntrinsic || !strings.HasPrefix(in.Intrin.Name, "atomic.") {
		return false
	}
	switch base, _ := atomicParts(in.Intrin.Name); base {
	case "load", "store", "fence":
		return false
	}
	return true
}

// isScratchReg reports whether r is one of the fixed scratch registers.
func isScratchReg(r Reg) bool {
	for _, s := range intScratchRegs {
		if r == s {
			return true
		}
	}
	return false
}

// freeScratch returns a fixed scratch register that is none of used, or ok=false
// when register pressure has claimed all three.
func freeScratch(used ...Reg) (Reg, bool) {
	for _, s := range intScratchRegs {
		free := true
		for _, u := range used {
			if s == u {
				free = false
				break
			}
		}
		if free {
			return s, true
		}
	}
	return 0, false
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
		if b.Jmp.To != s.next { // else fall through
			s.b.branch(b.Jmp.To)
		}
	case ir.JmpJnz:
		sz := s.f.ClassOf(b.Jmp.Arg).Size()
		r := s.src(b.Jmp.Arg, 0, sz)
		s.b.cbnz(sz == 8, r, b.Jmp.To)
		if b.Jmp.To2 != s.next { // else fall through
			s.b.branch(b.Jmp.To2)
		}
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

// triReg selects a three-source op. A spilled result reuses scratch slot 0, which
// a spilled first operand also holds; the fused instruction reads all its sources
// before writing the destination, so the overlap is harmless.
func (s *sel) triReg(in *ir.Instr, sz int, emit func(rd, rn, rm, ra Reg)) {
	s1 := s.src(in.Args[0], 0, sz)
	s2 := s.src(in.Args[1], 1, sz)
	s3 := s.src(in.Args[2], 2, sz)
	d, done := s.dst(in.To, sz)
	emit(d, s1, s2, s3)
	done()
}

// addSub emits an add/sub, folding a constant operand into a 12-bit immediate and
// flipping add<->sub for a negative constant.
func (s *sel) addSub(in *ir.Instr, sub bool) {
	sz := in.Cls.Size()
	w64 := sz == 8
	if in.Aux != 0 { // fuseArithShift folded a shift into Args[1]: add a, b, lsl #k
		sh, amt := shiftOp(in.Aux>>6), uint32(in.Aux&63)
		rn := s.src(in.Args[0], 0, sz)
		rm := s.src(in.Args[1], 1, sz)
		d, done := s.dst(in.To, sz)
		if sub {
			s.b.subShiftedReg(w64, sh, amt, d, rn, rm)
		} else {
			s.b.addShiftedReg(w64, sh, amt, d, rn, rm)
		}
		done()
		return
	}
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
	// fuseBitfield marks an AND that is an unsigned bitfield extract of a shifted
	// value (and(shr(x, lsb), 2^width-1)) with lsb/width packed in Aux.
	if op == logAnd && in.Aux != 0 {
		lsb, width := uint32(in.Aux>>8), uint32(in.Aux&0xff)
		rn := s.src(in.Args[0], 0, sz)
		d, done := s.dst(in.To, sz)
		s.b.ubfx(w64, d, rn, lsb, width)
		done()
		return
	}
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

// tstFlags emits an AND as a flags-only tst (ANDS into XZR), for a fused
// `if (x & mask)` branch: the mask result is never materialized into a register.
func (s *sel) tstFlags(in *ir.Instr) {
	s.tstFlagsPair(in.Args[0], in.Args[1])
}

// ccmpFlags emits a conjoined comparison (OCCmp) as flags-only: `cmp a1, b1 ;
// ccmp a2, b2, #nzcv, cond1`, so a following branch on the second predicate is
// taken exactly when cmp1 && cmp2 (or cmp1 || cmp2). The first compare sets the
// flags; the ccmp recomputes them from the second compare when cond1 selects it,
// else forwards a constant nzcv chosen so the branch reads the right way. Every
// materialization between the two is flags-preserving, so cmp1's flags reach the
// ccmp. Integer compares only (fuseCcmp gates floats out).
func (s *sel) ccmpFlags(in *ir.Instr) {
	pred1 := ir.Cmp(in.Aux >> 8)
	or := in.Aux&1 != 0
	cond1, _ := condOf(pred1, false)
	cond2, _ := condOf(in.Cmp, false)

	s.cmpFlagsPair(in.Args[0], in.Args[1]) // cmp a1, b1 (folds an immediate)

	ccmpCond := cond1
	nzcv := a64.FailNZCV(cond2) // &&: fail-forward so the branch is not taken
	if or {
		ccmpCond = cond1 ^ 1       // ||: run the ccmp when cond1 did NOT hold
		nzcv = a64.PassNZCV(cond2) // and pass-forward so the branch is taken
	}

	sz := s.f.ClassOf(in.Args[2]).Size()
	w64 := sz == 8
	a2, b2 := in.Args[2], in.Args[3]
	if v, ok := intConst(s.f, b2); ok && v >= 0 && v <= 31 {
		s.b.ccmpImm(w64, s.src(a2, 0, sz), uint32(v), nzcv, ccmpCond)
		return
	}
	r1 := s.src(a2, 0, sz)
	r2 := s.src(b2, 1, sz)
	s.b.ccmpReg(w64, r1, r2, nzcv, ccmpCond)
}

// tstFlagsPair is tstFlags over an explicit operand pair, so the fused
// compare-select (sel) can re-emit the mask test from an AND it absorbed. The
// width comes from the register operand's class (the arms may be wider or
// narrower than the mask test), and it uses scratch slots 0 and 1.
func (s *sel) tstFlagsPair(aRef, bRef ir.Ref) {
	if _, ok := intConst(s.f, aRef); ok {
		if _, ok := intConst(s.f, bRef); !ok {
			aRef, bRef = bRef, aRef // register operand -> aRef
		}
	}
	sz := s.f.ClassOf(aRef).Size()
	w64 := sz == 8
	if v, ok := intConst(s.f, bRef); ok {
		val := uint64(v)
		if sz == 4 {
			val &= 0xffffffff
		}
		s.b.tstImm(w64, s.src(aRef, 0, sz), val)
		return
	}
	r1 := s.src(aRef, 0, sz)
	r2 := s.src(bRef, 1, sz)
	s.b.tstReg(w64, r1, r2)
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
