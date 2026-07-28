package amd64

import "github.com/evanphx/cg12/ir"

// selectWide selects the 128-bit operations through the xasmWide half of the
// builder.
//
// The pair the family exists for is ir.OLoadq/ir.OStoreq: a 16-byte load and store,
// which SSE2 has exactly one instruction each for (see x64/sse2.go). They are also
// the only ops that can bring a 128-bit value (ir.ClsQ) into existence here, and on
// amd64 today nothing but hand-built IR asks for one. The System V classifier never
// produces a ClsQ (an aggregate is split into eightbytes, so a 16-byte struct moves
// as two ClsL halves), cc maps both __int128 and _Complex to ir.ClsP -- the address
// of their storage, not a 128-bit value -- and cc refuses `long double` for this
// target outright (cc/longdouble.go:36: amd64's is 80-bit x87, not the AArch64
// IEEE quad the soft-float helpers compute). So this family adds a capability
// rather than changing how anything already-working is compiled.
//
// It also claims the three ops that would otherwise *move* such a value with a
// scalar instruction, which is why they are here rather than left to selectCore. A
// ClsQ temp reports IsFloat, so every generic float path sizes it with w64(16) ==
// false and emits MOVSS -- a four-byte move for a sixteen-byte value, silently
// discarding twelve of them. That was unreachable while OLoadq errored out; landing
// the load makes it reachable, so the moves have to be widened in the same change:
//
//   - ir.OCopy: SSA destruction's phi copy (lower/ssa.go:330 takes its class from
//     the temp), which becomes a whole-vector MOVDQA between registers.
//   - ir.OSpill/ir.OReload: the caller-save pair around a call the value is live
//     across. These cannot be widened, only refused -- see wideReg.
func selectWide(s *xsel, in *ir.Instr) bool {
	switch in.Op {
	case ir.OLoadq:
		if dst, ok := s.wideReg(in.To, in.Op); ok {
			s.b.loadWide(in, dst)
		}
		return true
	case ir.OStoreq:
		if val, ok := s.wideReg(in.Arg(0), in.Op); ok {
			s.b.storeWide(in, val)
		}
		return true
	case ir.OCopy:
		if in.Cls != ir.ClsQ {
			return false // a scalar copy is selectCore's move
		}
		dst, dstOK := s.wideReg(in.To, in.Op)
		src, srcOK := s.wideReg(in.Arg(0), in.Op)
		if dstOK && srcOK {
			s.b.movWide(dst, src)
		}
		return true
	case ir.OSpill, ir.OReload:
		if in.Cls != ir.ClsQ {
			return false // a scalar save/restore is the emitter's own spillReg/reloadReg
		}
		s.wideSlotUnsupported(in.Op)
		return true
	}
	return false
}

// wideReg returns the XMM register a 128-bit operand lives in.
//
// A 128-bit value must be register-resident, because amd64 has no 16-byte spill
// slot to put one in: every slot is eight bytes, fixed at the two places that hand
// them out (gcalloc.go's spill, callersave.go's slot) and again in
// frameLayout.slotAddr, whose "+8" is the slot stride. Storing sixteen bytes at a
// slot address would run over the neighbouring slot -- or, for slot 0, over the
// saved callee registers. So an operand that is not in a register is refused here,
// with a message naming the reason, rather than encoded into a MOVDQU that corrupts
// the frame. arm64 sizes its slots per class (arm64/gcalloc.go:313-320,
// arm64/callersave.go:92-97) and has no such restriction; matching that is a frame
// and allocator change, in files this family does not own.
//
// The restriction is invisible for the code that produces 128-bit values today:
// they are short-lived, and fourteen XMM registers are allocatable. It bites in two
// situations -- XMM pressure high enough to spill one, and a value live across a
// call, which System V gives no callee-saved XMM to keep it in, so colouring hands
// it a slot.
func (s *xsel) wideReg(ref ir.Ref, op ir.Op) (Reg, bool) {
	l := s.b.refLoc(ref)
	if l.kind == locReg {
		return l.reg, true
	}
	s.b.fail("amd64: %s needs its 128-bit operand in an XMM register; "+
		"a spilled one has only an 8-byte frame slot", op)
	return 0, false
}

// wideSlotUnsupported refuses a caller-save spill or reload of a 128-bit value.
// The emitter's own spillReg/reloadReg would size it by cls.Size(), find 16 in a
// switch that tests for 8 and then anything-else, and take the MOVSS branch --
// writing four bytes of sixteen. There is no correct encoding to substitute,
// because the slot the pair was given is eight bytes wide (see wideReg), so
// refusing is the only honest answer. It is a better one than a truncation that
// surfaces much later as a wrong value.
func (s *xsel) wideSlotUnsupported(op ir.Op) {
	s.b.fail("amd64: %s of a 128-bit value is unsupported; "+
		"a frame slot is 8 bytes, so the value cannot be saved across a call", op)
}
