package amd64

import "github.com/evanphx/cg12/amd64/x64"

// xasmBits is the bit-manipulation surface: the leading-zero count behind OClz,
// and the high half of a widening multiply for OUMulh / OSMulh.
//
// Both are multi-instruction idioms rather than single encodings, for the same
// reason divGP is: x86-64 states them in terms of a fixed register pair or a
// flags-only result, so the correction that turns the machine's answer into the
// IR's answer belongs next to the encoding it corrects, not in the selector.
//
// What is *not* here is as deliberate as what is. POPCNT, TZCNT, BSWAP, SHLD /
// SHRD, ROL / ROR and the BT family all have obvious x86-64 encodings and no IR
// op that would ever reach them: cg12 synthesizes population count with the SWAR
// sequence, trailing-zero count from OClz of the isolated low bit, byte swap and
// rotate from shifts and ors (see cc/builtin.go), and has no double-shift or
// bit-test op at all. ORotr exists but is produced only by arm64's own lowering
// (arm64/lower.go foldRotate) and is marked private for that reason, so amd64
// never sees one either. An encoder with no producer is untested code that reads
// as though it were reachable, so they stay out until an op arrives.
type xasmBits interface {
	// clzGP writes the number of leading zero bits of src -- at 32 or 64 bits per
	// w -- into dst, using tmp and aux as scratch.
	//
	// dst may be the same register as src, since src is read only by the first
	// instruction of the sequence. tmp and aux must be distinct from each other
	// and from both dst and src: tmp carries the corrected bit index across the
	// final subtraction, and aux carries the constant the conditional move reads.
	clzGP(w bool, dst, src, tmp, aux Reg)

	// mulhGP multiplies RAX (already set up by the caller) by src and leaves the
	// full double-width product in RDX:RAX -- so the high half that OUMulh and
	// OSMulh want lands in RDX, and the low half OMul would have computed is in
	// RAX. signed selects IMUL over MUL, which is not a relabelling: the two agree
	// on the low half and differ in the high one, which is precisely where the
	// sign lives. w narrows the pair to EDX:EAX.
	mulhGP(w, signed bool, src Reg)
}

// clzGP counts leading zeros with BSR and a conditional move.
//
// BSR reports the *index* of the most significant set bit, so the count is
// (width-1) - index; and for a zero source its destination is architecturally
// undefined while ZF is set, so the zero case has to come from the flags. Feeding
// the correction -1 through CMOVE before the subtraction gets both from one
// sequence: (width-1) - (-1) is width, which is what OClz of zero is defined to
// be (ir.OClz is bits.LeadingZeros{32,64}; see interp/ops.go, and arm64's CLZ
// agrees natively).
//
// The subtraction is what forces the conditional move to come first: SUB writes
// flags, so there is no ordering in which ZF from BSR survives long enough to
// select between two already-computed answers. CMOV also has no immediate form,
// hence aux holding the -1.
//
// The alternative would be LZCNT, which is this whole sequence in one
// instruction and defines its answer for zero. It is not used, and the reason is
// not conservatism: LZCNT is CPU-feature-gated (BMI1/ABM), cg12 has no target
// feature model to gate it on, and its encoding is BSR plus an F3 prefix that an
// older CPU silently ignores -- so guessing wrong yields a program that computes
// the wrong number rather than one that faults. See x64/bits.go.
func (b *mcXasm) clzGP(w bool, dst, src, tmp, aux Reg) {
	width := int32(32)
	if w {
		width = 64
	}
	b.m.emit(x64.Bsr(w, tmp.mreg(), src.mreg()))
	// MOV does not touch flags, so ZF from the BSR above is still live at the CMOV.
	b.m.emit(x64.MovImm32(w, aux.mreg(), -1))
	b.m.emit(x64.Cmovcc(w, x64.E, tmp.mreg(), aux.mreg()))
	b.m.emit(x64.MovImm32(w, dst.mreg(), width-1))
	b.m.emit(x64.SubReg(w, dst.mreg(), tmp.mreg()))
}

func (b *mcXasm) mulhGP(w, signed bool, src Reg) {
	if signed {
		b.m.emit(x64.ImulWide(w, src.mreg()))
	} else {
		b.m.emit(x64.MulWide(w, src.mreg()))
	}
}

// clzScratch and clzAux are the two scratch registers the OClz sequence needs
// beyond its source and destination. RCX and RDX are held out of allocation
// (reg.go's intAllocOrder) for exactly this class of use -- the fixed-register
// instructions -- so nothing live can be sitting in them, the same standing
// arrangement divGP relies on for RDX:RAX and the variable shift relies on for
// CL. Naming them here rather than writing RCX and RDX at the one call site keeps
// the choice reviewable alongside the reservation that justifies it.
const (
	clzScratch = RCX
	clzAux     = RDX
)
