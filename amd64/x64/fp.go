package x64

// SSE scalar floating-point encodings. Scalar-double instructions carry a
// mandatory 0xF2 prefix, scalar-single a 0xF3 prefix, and the "packed"
// compare/move helpers a 0x66 (or no) prefix; the mandatory prefix precedes any
// REX byte. XMM registers reuse the 0..15 numbering, riding the same REX bits.

var (
	pfxSD = []byte{0xf2}
	pfxSS = []byte{0xf3}
	pfx66 = []byte{0x66}
)

// --- moves -----------------------------------------------------------------

// MovsdReg / MovssReg copy one scalar between XMM registers (10 /r, load form).
func MovsdReg(dst, src Reg) []byte {
	return op_rr(nil, pfxSD, false, []byte{0x0f, 0x10}, dst, src, false)
}
func MovssReg(dst, src Reg) []byte {
	return op_rr(nil, pfxSS, false, []byte{0x0f, 0x10}, dst, src, false)
}

// MovsdLoad / MovssLoad load a scalar from memory into an XMM register.
func MovsdLoad(dst Reg, m Mem) []byte {
	return op_rm(nil, pfxSD, false, []byte{0x0f, 0x10}, dst, m, false)
}
func MovssLoad(dst Reg, m Mem) []byte {
	return op_rm(nil, pfxSS, false, []byte{0x0f, 0x10}, dst, m, false)
}

// MovsdStore / MovssStore store a scalar XMM register to memory (11 /r).
func MovsdStore(src Reg, m Mem) []byte {
	return op_rm(nil, pfxSD, false, []byte{0x0f, 0x11}, src, m, false)
}
func MovssStore(src Reg, m Mem) []byte {
	return op_rm(nil, pfxSS, false, []byte{0x0f, 0x11}, src, m, false)
}

// --- arithmetic ------------------------------------------------------------

func sseRR(pfx []byte, op byte, dst, src Reg) []byte {
	return op_rr(nil, pfx, false, []byte{0x0f, op}, dst, src, false)
}

// Scalar-double arithmetic (dst op= src).
func Addsd(dst, src Reg) []byte { return sseRR(pfxSD, 0x58, dst, src) }
func Subsd(dst, src Reg) []byte { return sseRR(pfxSD, 0x5c, dst, src) }
func Mulsd(dst, src Reg) []byte { return sseRR(pfxSD, 0x59, dst, src) }
func Divsd(dst, src Reg) []byte { return sseRR(pfxSD, 0x5e, dst, src) }

// Scalar-single arithmetic.
func Addss(dst, src Reg) []byte { return sseRR(pfxSS, 0x58, dst, src) }
func Subss(dst, src Reg) []byte { return sseRR(pfxSS, 0x5c, dst, src) }
func Mulss(dst, src Reg) []byte { return sseRR(pfxSS, 0x59, dst, src) }
func Divss(dst, src Reg) []byte { return sseRR(pfxSS, 0x5e, dst, src) }

// --- compare ---------------------------------------------------------------

// Ucomisd / Ucomiss set EFLAGS from an unordered scalar compare (dst ? src); a
// following Jcc/Setcc reads ZF/CF/PF.
func Ucomisd(dst, src Reg) []byte {
	return op_rr(nil, pfx66, false, []byte{0x0f, 0x2e}, dst, src, false)
}
func Ucomiss(dst, src Reg) []byte { return op_rr(nil, nil, false, []byte{0x0f, 0x2e}, dst, src, false) }

// --- bitwise (zeroing and sign flips) --------------------------------------

// Xorps zeroes or flips XMM bits (0F 57 /r); Xorpd is the 66-prefixed form used
// with a sign-bit mask to negate a double.
func Xorps(dst, src Reg) []byte { return op_rr(nil, nil, false, []byte{0x0f, 0x57}, dst, src, false) }
func Xorpd(dst, src Reg) []byte { return op_rr(nil, pfx66, false, []byte{0x0f, 0x57}, dst, src, false) }

// --- conversions -----------------------------------------------------------

// Cvtsi2sd / Cvtsi2ss convert a signed integer (GPR src) to a scalar float in
// dst; w selects a 64-bit source integer.
func Cvtsi2sd(w bool, dst, src Reg) []byte {
	return op_rr(nil, pfxSD, w, []byte{0x0f, 0x2a}, dst, src, false)
}
func Cvtsi2ss(w bool, dst, src Reg) []byte {
	return op_rr(nil, pfxSS, w, []byte{0x0f, 0x2a}, dst, src, false)
}

// Cvttsd2si / Cvttss2si convert a scalar float (XMM src) to a signed integer in
// dst with truncation; w selects a 64-bit destination integer.
func Cvttsd2si(w bool, dst, src Reg) []byte {
	return op_rr(nil, pfxSD, w, []byte{0x0f, 0x2c}, dst, src, false)
}
func Cvttss2si(w bool, dst, src Reg) []byte {
	return op_rr(nil, pfxSS, w, []byte{0x0f, 0x2c}, dst, src, false)
}

// Cvtsd2ss / Cvtss2sd convert between double and single precision.
func Cvtsd2ss(dst, src Reg) []byte { return sseRR(pfxSD, 0x5a, dst, src) }
func Cvtss2sd(dst, src Reg) []byte { return sseRR(pfxSS, 0x5a, dst, src) }

// --- GPR <-> XMM bit moves -------------------------------------------------

// MovqToXmm moves the bits of a GPR (src) into an XMM register (66 REX.W 0F 6E);
// with w=false it is MOVD (32-bit).
func MovqToXmm(w bool, dst, src Reg) []byte {
	return op_rr(nil, pfx66, w, []byte{0x0f, 0x6e}, dst, src, false)
}

// MovqFromXmm moves the bits of an XMM register (src) into a GPR (66 REX.W 0F 7E).
func MovqFromXmm(w bool, dst, src Reg) []byte {
	return op_rr(nil, pfx66, w, []byte{0x0f, 0x7e}, src, dst, false)
}
