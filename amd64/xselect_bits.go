package amd64

import "github.com/evanphx/cg12/ir"

// selectBits selects the bit-manipulation operations: the leading-zero count
// (ir.OClz, backing __builtin_clz / __builtin_clzll and, through the isolated
// low bit, __builtin_ctz) and the high half of a widening multiply (ir.OUMulh /
// ir.OSMulh, which is how an overflowing 64-bit multiply is detected without a
// division).
//
// All three would otherwise reach mc.go's "unsupported op". None of them is
// claimed by selectCore, so this family overrides nothing and its position in the
// probe chain is free.
func selectBits(s *xsel, in *ir.Instr) bool {
	switch in.Op {
	case ir.OClz:
		s.clz(in)
	case ir.OUMulh:
		s.mulh(in, false)
	case ir.OSMulh:
		s.mulh(in, true)
	default:
		return false
	}
	return true
}

// clz emits a leading-zero count. in.Cls gives both the operand width and the
// result width, matching ir.OClz's definition (and the interpreter's, which reads
// the same field).
//
// The source register is read before either scratch or the destination is
// written, so dst is allowed to be the same register as src -- which it may be,
// since nothing here asks the allocator to keep them apart. clzScratch and clzAux
// are RCX and RDX, which the allocator never hands out, so they cannot collide
// with src or dst however those were assigned.
func (s *xsel) clz(in *ir.Instr) {
	w := in.Cls == ir.ClsL
	rs := s.gpValue(in.Arg(0), s.gpScratch1)
	d, commit := s.gpDst(in.To)
	s.b.clzGP(w, d, rs, clzScratch, clzAux)
	commit()
}

// mulh emits the high half of a widening multiply. The shape is div's, because
// the constraint is the same one: the instruction is written in terms of the
// RDX:RAX pair rather than in terms of operands the allocator chose. So the
// multiplicand is moved into RAX, the multiplier is taken from wherever it
// already is (or staged in a scratch register), and the result is read out of
// RDX -- the half the low-half OMul discards.
//
// Nothing is needed to keep RAX and RDX free: reg.go leaves both out of
// intAllocOrder precisely so that these fixed-register instructions can be
// emitted without a reservation pass, which is also what lets divGP write them
// unconditionally.
//
// The multiplier register cannot be RAX or RDX and so cannot be clobbered before
// it is read: it comes from the allocator or from gpScratch1, and neither
// produces those two.
func (s *xsel) mulh(in *ir.Instr, signed bool) {
	w := in.Cls == ir.ClsL
	rb := s.gpValue(in.Arg(1), s.gpScratch1)
	s.gpInto(RAX, in.Arg(0))
	s.b.mulhGP(w, signed, rb)
	d, commit := s.gpDst(in.To)
	s.b.movReg(w, d, RDX)
	commit()
}
