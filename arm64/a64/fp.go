package a64

// AArch64 scalar floating-point instructions. FP registers use the same 5-bit
// field as integer registers (0..31 for S0..S31 / D0..D31); dbl selects the
// double (D) variant over single (S).

func ftype(dbl bool) uint32 {
	if dbl {
		return 1
	}
	return 0
}

// --- FP data processing (2 source): fadd/fsub/fmul/fdiv ---------------------

func fpDP2(dbl bool, opcode uint32, rd, rn, rm Reg) uint32 {
	return 0x1e<<24 | ftype(dbl)<<22 | 1<<21 | r(rm)<<16 | opcode<<12 | 0b10<<10 | r(rn)<<5 | r(rd)
}

func Fadd(dbl bool, rd, rn, rm Reg) uint32 { return fpDP2(dbl, 0b0010, rd, rn, rm) }
func Fsub(dbl bool, rd, rn, rm Reg) uint32 { return fpDP2(dbl, 0b0011, rd, rn, rm) }
func Fmul(dbl bool, rd, rn, rm Reg) uint32 { return fpDP2(dbl, 0b0000, rd, rn, rm) }
func Fdiv(dbl bool, rd, rn, rm Reg) uint32 { return fpDP2(dbl, 0b0001, rd, rn, rm) }

// --- FP data processing (1 source): fneg/fmov(reg)/fcvt ---------------------

func fpDP1(ft, opcode uint32, rd, rn Reg) uint32 {
	return 0x1e<<24 | ft<<22 | 1<<21 | opcode<<15 | 0b10000<<10 | r(rn)<<5 | r(rd)
}

// Fneg encodes FNEG rd, rn.
func Fneg(dbl bool, rd, rn Reg) uint32 { return fpDP1(ftype(dbl), 0b000010, rd, rn) }

// Fabs encodes FABS rd, rn: the magnitude of rn, which is the sign bit cleared
// and every other bit -- NaN payloads included -- carried through.
func Fabs(dbl bool, rd, rn Reg) uint32 { return fpDP1(ftype(dbl), 0b000001, rd, rn) }

// Fsqrt encodes FSQRT rd, rn: the correctly-rounded IEEE 754 square root.
func Fsqrt(dbl bool, rd, rn Reg) uint32 { return fpDP1(ftype(dbl), 0b000011, rd, rn) }

// The FRINT family rounds to an integral value in the floating-point format,
// each with the rounding mode named in the mnemonic rather than the one in
// FPCR. That is what makes them usable as Go's math.Floor and friends, whose
// rounding is fixed by the specification and not by the machine's state.
//
//	Frintn -- to nearest, ties to even   (math.RoundToEven)
//	Frintp -- toward +Inf                (math.Ceil)
//	Frintm -- toward -Inf                (math.Floor)
//	Frintz -- toward zero                (math.Trunc)
//	Frinta -- to nearest, ties away from zero (math.Round)
func Frintn(dbl bool, rd, rn Reg) uint32 { return fpDP1(ftype(dbl), 0b001000, rd, rn) }
func Frintp(dbl bool, rd, rn Reg) uint32 { return fpDP1(ftype(dbl), 0b001001, rd, rn) }
func Frintm(dbl bool, rd, rn Reg) uint32 { return fpDP1(ftype(dbl), 0b001010, rd, rn) }
func Frintz(dbl bool, rd, rn Reg) uint32 { return fpDP1(ftype(dbl), 0b001011, rd, rn) }
func Frinta(dbl bool, rd, rn Reg) uint32 { return fpDP1(ftype(dbl), 0b001100, rd, rn) }

// FmovReg encodes FMOV rd, rn (register-to-register float copy).
func FmovReg(dbl bool, rd, rn Reg) uint32 { return fpDP1(ftype(dbl), 0b000000, rd, rn) }

// MovVec16b encodes MOV Vd.16B, Vn.16B (a full 128-bit SIMD register copy, an
// alias of ORR Vd.16B, Vn.16B, Vn.16B).
func MovVec16b(rd, rn Reg) uint32 { return 0x4ea01c00 | r(rn)<<16 | r(rn)<<5 | r(rd) }

// FcvtStoD encodes FCVT Dd, Sn (single -> double).
func FcvtStoD(rd, rn Reg) uint32 { return fpDP1(0, 0b000101, rd, rn) }

// FcvtDtoS encodes FCVT Sd, Dn (double -> single).
func FcvtDtoS(rd, rn Reg) uint32 { return fpDP1(1, 0b000100, rd, rn) }

// --- FP compare ------------------------------------------------------------

// Fcmp encodes FCMP rn, rm (sets the flags).
func Fcmp(dbl bool, rn, rm Reg) uint32 {
	return 0x1e<<24 | ftype(dbl)<<22 | 1<<21 | r(rm)<<16 | 0b001000<<10 | r(rn)<<5
}

// Fcsel encodes FCSEL rd, rn, rm, cond (rd = cond ? rn : rm).
func Fcsel(dbl bool, rd, rn, rm Reg, c Cond) uint32 {
	return 0x1e<<24 | ftype(dbl)<<22 | 1<<21 | r(rm)<<16 | uint32(c)<<12 | 0b11<<10 | r(rn)<<5 | r(rd)
}

// --- conversions between FP and integer ------------------------------------

func fpIntConv(sfb, ft, rmode, opcode uint32, rd, rn Reg) uint32 {
	return sfb<<31 | 0x1e<<24 | ft<<22 | 1<<21 | rmode<<19 | opcode<<16 | r(rn)<<5 | r(rd)
}

// Fcvtzs / Fcvtzu convert an FP value to a signed/unsigned integer, rounding
// toward zero. dstW64 picks the X (64-bit) destination; srcDbl the D source.
func Fcvtzs(dstW64, srcDbl bool, rd, rn Reg) uint32 {
	return fpIntConv(sf(dstW64), ftype(srcDbl), 0b11, 0b000, rd, rn)
}
func Fcvtzu(dstW64, srcDbl bool, rd, rn Reg) uint32 {
	return fpIntConv(sf(dstW64), ftype(srcDbl), 0b11, 0b001, rd, rn)
}

// Scvtf / Ucvtf convert a signed/unsigned integer to an FP value. dstDbl picks
// the D destination; srcW64 the X (64-bit) integer source.
func Scvtf(dstDbl, srcW64 bool, rd, rn Reg) uint32 {
	return fpIntConv(sf(srcW64), ftype(dstDbl), 0, 0b010, rd, rn)
}
func Ucvtf(dstDbl, srcW64 bool, rd, rn Reg) uint32 {
	return fpIntConv(sf(srcW64), ftype(dstDbl), 0, 0b011, rd, rn)
}

// FmovFromGP moves an integer register's bits into an FP register (FMOV Sd, Wn /
// FMOV Dd, Xn); FmovToGP does the reverse. dbl selects the 64-bit (D/X) width.
func FmovFromGP(dbl bool, rd, rn Reg) uint32 { return fpIntConv(sf(dbl), ftype(dbl), 0, 0b111, rd, rn) }
func FmovToGP(dbl bool, rd, rn Reg) uint32   { return fpIntConv(sf(dbl), ftype(dbl), 0, 0b110, rd, rn) }
