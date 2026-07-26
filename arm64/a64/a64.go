// Package a64 encodes AArch64 (A64) instructions directly into machine-code
// bytes, so the backend need not shell out to an external assembler. Every A64
// instruction is a fixed 32-bit little-endian word; each function here returns
// that word for one instruction form. Registers are the raw 5-bit encodings
// (0..30 for X0..X30/W0..W30, 31 for the stack pointer or zero register), and a
// bool selects the 64-bit (X) or 32-bit (W) variant.
//
// The encodings are validated byte-for-byte against a reference assembler in the
// tests; the reference is used only to check correctness, never at runtime.
package a64

import "fmt"

// field checks that v fits in the low `bits` bits and returns it. An
// out-of-range value is a compiler bug — silently masking it would emit a
// valid-looking instruction that addresses the wrong thing (e.g. a frame offset
// above 4095 wrapping into another slot), so we panic loudly instead.
func field(v uint32, bits uint, what string) uint32 {
	if v>>bits != 0 {
		panic(fmt.Sprintf("a64: %s immediate %d (%#x) does not fit in %d bits", what, v, v, bits))
	}
	return v
}

// scaled divides a byte offset by its access width, checking it divides evenly
// and fits the 12-bit unsigned-offset field.
func scaled(off, scale uint32, what string) uint32 {
	if off%scale != 0 {
		panic(fmt.Sprintf("a64: %s offset %d is not a multiple of %d", what, off, scale))
	}
	return field(off/scale, 12, what)
}

// sfield checks that a signed value fits a `bits`-wide two's-complement field and
// returns its low bits. Masking the two's-complement form is intentional; a value
// outside the representable range is a bug.
func sfield(v int32, bits uint, what string) uint32 {
	if v < -(1<<(bits-1)) || v >= 1<<(bits-1) {
		panic(fmt.Sprintf("a64: %s offset %d does not fit in a signed %d-bit field", what, v, bits))
	}
	return uint32(v) & (1<<bits - 1)
}

// branchOff checks a PC-relative byte offset (a multiple of 4) and returns the
// signed instruction-count field of the given width.
func branchOff(off int32, bits uint, what string) uint32 {
	if off%4 != 0 {
		panic(fmt.Sprintf("a64: %s offset %d is not a multiple of 4", what, off))
	}
	return sfield(off/4, bits, what)
}

// Reg is a raw 5-bit register number (0..31).
type Reg uint32

// Register 31 is context-dependent: the stack pointer or the zero register.
const (
	SP Reg = 31
	ZR Reg = 31
)

// sf returns the size flag: 1 for a 64-bit operation, 0 for 32-bit.
func sf(w64 bool) uint32 {
	if w64 {
		return 1
	}
	return 0
}

func r(x Reg) uint32 { return field(uint32(x), 5, "reg") }

// --- data processing: add/subtract ----------------------------------------

func addSubReg(w64 bool, op, s uint32, rd, rn, rm Reg) uint32 {
	return sf(w64)<<31 | op<<30 | s<<29 | 0x0b<<24 | r(rm)<<16 | r(rn)<<5 | r(rd)
}

// AddReg encodes ADD rd, rn, rm.
func AddReg(w64 bool, rd, rn, rm Reg) uint32 { return addSubReg(w64, 0, 0, rd, rn, rm) }

// MrsTPIDR encodes MRS Xt, TPIDR_EL0: read the thread pointer, the base for
// thread-local storage addressing.
func MrsTPIDR(rt Reg) uint32 { return 0xd53bd040 | r(rt) }

// AddExtSxtw encodes ADD Xd, Xn, Wm, SXTW: the 32-bit Wm operand is
// sign-extended to 64 bits before the add. Used to advance a pointer by a signed
// 32-bit offset (as in the variadic argument walk).
func AddExtSxtw(rd, rn, rm Reg) uint32 {
	const option = 6 // SXTW
	return 1<<31 | 0x0b<<24 | 1<<21 | r(rm)<<16 | option<<13 | r(rn)<<5 | r(rd)
}

// SubReg encodes SUB rd, rn, rm.
func SubReg(w64 bool, rd, rn, rm Reg) uint32 { return addSubReg(w64, 1, 0, rd, rn, rm) }

// addSubShiftedReg encodes add/sub with rm shifted by a constant: shift selects
// LSL (0), LSR (1), or ASR (2); amount is 0..63 (0..31 for a W operand).
func addSubShiftedReg(w64 bool, op, shift, amount uint32, rd, rn, rm Reg) uint32 {
	return sf(w64)<<31 | op<<30 | 0x0b<<24 | shift<<22 |
		r(rm)<<16 | field(amount, 6, "shift amount")<<10 | r(rn)<<5 | r(rd)
}

// AddShiftedReg encodes ADD rd, rn, rm, {lsl|lsr|asr} #amount.
func AddShiftedReg(w64 bool, shift, amount uint32, rd, rn, rm Reg) uint32 {
	return addSubShiftedReg(w64, 0, shift, amount, rd, rn, rm)
}

// SubShiftedReg encodes SUB rd, rn, rm, {lsl|lsr|asr} #amount.
func SubShiftedReg(w64 bool, shift, amount uint32, rd, rn, rm Reg) uint32 {
	return addSubShiftedReg(w64, 1, shift, amount, rd, rn, rm)
}

// SubsReg encodes SUBS rd, rn, rm (sets flags); CmpReg is SUBS ZR, rn, rm.
func SubsReg(w64 bool, rd, rn, rm Reg) uint32 { return addSubReg(w64, 1, 1, rd, rn, rm) }

// CmpReg encodes CMP rn, rm.
func CmpReg(w64 bool, rn, rm Reg) uint32 { return SubsReg(w64, ZR, rn, rm) }

func addSubImm(w64 bool, op, s uint32, rd, rn Reg, imm12 uint32) uint32 {
	return sf(w64)<<31 | op<<30 | s<<29 | 0x11<<24 | field(imm12, 12, "add/sub")<<10 | r(rn)<<5 | r(rd)
}

// AddImm encodes ADD rd, rn, #imm12 (0..4095, unshifted).
func AddImm(w64 bool, rd, rn Reg, imm12 uint32) uint32 { return addSubImm(w64, 0, 0, rd, rn, imm12) }

// SubImm encodes SUB rd, rn, #imm12.
func SubImm(w64 bool, rd, rn Reg, imm12 uint32) uint32 { return addSubImm(w64, 1, 0, rd, rn, imm12) }

// --- data processing: logical ---------------------------------------------

func logicalReg(w64 bool, opc uint32, rd, rn, rm Reg) uint32 {
	return sf(w64)<<31 | opc<<29 | 0x0a<<24 | r(rm)<<16 | r(rn)<<5 | r(rd)
}

// AndReg encodes AND rd, rn, rm.
func AndReg(w64 bool, rd, rn, rm Reg) uint32 { return logicalReg(w64, 0, rd, rn, rm) }

// OrrReg encodes ORR rd, rn, rm.
func OrrReg(w64 bool, rd, rn, rm Reg) uint32 { return logicalReg(w64, 1, rd, rn, rm) }

// EorReg encodes EOR rd, rn, rm.
func EorReg(w64 bool, rd, rn, rm Reg) uint32 { return logicalReg(w64, 2, rd, rn, rm) }

// MovReg encodes MOV rd, rm (ORR rd, ZR, rm).
func MovReg(w64 bool, rd, rm Reg) uint32 { return OrrReg(w64, rd, ZR, rm) }

func logicalRegN(w64 bool, opc, n uint32, rd, rn, rm Reg) uint32 {
	return sf(w64)<<31 | opc<<29 | 0x0a<<24 | field(n, 1, "logical N")<<21 | r(rm)<<16 | r(rn)<<5 | r(rd)
}

// BicReg encodes BIC rd, rn, rm (rd = rn AND NOT rm).
func BicReg(w64 bool, rd, rn, rm Reg) uint32 { return logicalRegN(w64, 0, 1, rd, rn, rm) }

// OrnReg encodes ORN rd, rn, rm (rd = rn OR NOT rm).
func OrnReg(w64 bool, rd, rn, rm Reg) uint32 { return logicalRegN(w64, 1, 1, rd, rn, rm) }

// MvnReg encodes MVN rd, rm (ORN rd, ZR, rm) — the bitwise NOT.
func MvnReg(w64 bool, rd, rm Reg) uint32 { return OrnReg(w64, rd, ZR, rm) }

// --- move wide immediate ---------------------------------------------------

func moveWide(w64 bool, opc uint32, rd Reg, imm16 uint16, shift uint32) uint32 {
	if shift%16 != 0 {
		panic(fmt.Sprintf("a64: movw shift %d is not a multiple of 16", shift))
	}
	hw := field(shift/16, 2, "movw shift")
	return sf(w64)<<31 | opc<<29 | 0x25<<23 | hw<<21 | uint32(imm16)<<5 | r(rd)
}

// Movz encodes MOVZ rd, #imm16, LSL #shift (shift in {0,16,32,48}).
func Movz(w64 bool, rd Reg, imm16 uint16, shift uint32) uint32 {
	return moveWide(w64, 2, rd, imm16, shift)
}

// Movk encodes MOVK rd, #imm16, LSL #shift.
func Movk(w64 bool, rd Reg, imm16 uint16, shift uint32) uint32 {
	return moveWide(w64, 3, rd, imm16, shift)
}

// Movn encodes MOVN rd, #imm16, LSL #shift.
func Movn(w64 bool, rd Reg, imm16 uint16, shift uint32) uint32 {
	return moveWide(w64, 0, rd, imm16, shift)
}

// --- data processing (2 source): division and variable shifts -------------

func dataProc2(w64 bool, opcode uint32, rd, rn, rm Reg) uint32 {
	return sf(w64)<<31 | 0xd6<<21 | r(rm)<<16 | opcode<<10 | r(rn)<<5 | r(rd)
}

// dataProc1 encodes a one-source data-processing instruction (e.g. CLZ).
func dataProc1(w64 bool, opcode uint32, rd, rn Reg) uint32 {
	return sf(w64)<<31 | 1<<30 | 0xd6<<21 | field(opcode, 6, "dp1 opcode")<<10 | r(rn)<<5 | r(rd)
}

// Clz encodes CLZ rd, rn (count leading zeros).
func Clz(w64 bool, rd, rn Reg) uint32 { return dataProc1(w64, 0b000100, rd, rn) }

// Udiv encodes UDIV rd, rn, rm.
func Udiv(w64 bool, rd, rn, rm Reg) uint32 { return dataProc2(w64, 0b000010, rd, rn, rm) }

// Sdiv encodes SDIV rd, rn, rm.
func Sdiv(w64 bool, rd, rn, rm Reg) uint32 { return dataProc2(w64, 0b000011, rd, rn, rm) }

// Lslv encodes LSLV rd, rn, rm (variable left shift).
func Lslv(w64 bool, rd, rn, rm Reg) uint32 { return dataProc2(w64, 0b001000, rd, rn, rm) }

// Lsrv encodes LSRV rd, rn, rm.
func Lsrv(w64 bool, rd, rn, rm Reg) uint32 { return dataProc2(w64, 0b001001, rd, rn, rm) }

// Asrv encodes ASRV rd, rn, rm.
func Asrv(w64 bool, rd, rn, rm Reg) uint32 { return dataProc2(w64, 0b001010, rd, rn, rm) }

// --- data processing (3 source): multiply/multiply-add --------------------

func dataProc3(w64 bool, o0 uint32, rd, rn, rm, ra Reg) uint32 {
	return sf(w64)<<31 | 0x1b<<24 | r(rm)<<16 | o0<<15 | r(ra)<<10 | r(rn)<<5 | r(rd)
}

// Madd encodes MADD rd, rn, rm, ra (rd = ra + rn*rm); Mul is Madd with ra=ZR.
func Madd(w64 bool, rd, rn, rm, ra Reg) uint32 { return dataProc3(w64, 0, rd, rn, rm, ra) }

// Msub encodes MSUB rd, rn, rm, ra (rd = ra - rn*rm).
func Msub(w64 bool, rd, rn, rm, ra Reg) uint32 { return dataProc3(w64, 1, rd, rn, rm, ra) }

// Mul encodes MUL rd, rn, rm.
func Mul(w64 bool, rd, rn, rm Reg) uint32 { return Madd(w64, rd, rn, rm, ZR) }

// Umulh encodes UMULH rd, rn, rm: rd = high 64 bits of the unsigned product
// rn*rm. It is a widening multiply (op31 = 0b110, Ra = ZR), 64-bit only.
func Umulh(rd, rn, rm Reg) uint32 {
	return 1<<31 | 0x1b<<24 | 0b110<<21 | r(rm)<<16 | r(ZR)<<10 | r(rn)<<5 | r(rd)
}

// Smulh encodes SMULH rd, rn, rm: rd = high 64 bits of the signed product
// rn*rm (op31 = 0b010, Ra = ZR), 64-bit only.
func Smulh(rd, rn, rm Reg) uint32 {
	return 1<<31 | 0x1b<<24 | 0b010<<21 | r(rm)<<16 | r(ZR)<<10 | r(rn)<<5 | r(rd)
}

// --- conditional select ----------------------------------------------------

// Cond is an AArch64 condition code.
type Cond uint32

const (
	EQ Cond = 0
	NE Cond = 1
	CS Cond = 2
	CC Cond = 3
	MI Cond = 4
	PL Cond = 5
	VS Cond = 6
	VC Cond = 7
	HI Cond = 8
	LS Cond = 9
	GE Cond = 10
	LT Cond = 11
	GT Cond = 12
	LE Cond = 13
)

// invert flips a condition, as CSET needs (CSET uses the inverted condition).
func (c Cond) invert() Cond { return c ^ 1 }

// Invert returns the exact complement condition: a branch on Invert(c) is taken in
// precisely the NZCV states where a branch on c is not, so `b.c To; fall To2` and
// `b.Invert(c) To2; fall To` are equivalent (true for float/unordered too, since it
// complements the flag test, not the comparison).
func (c Cond) Invert() Cond { return c ^ 1 }

// Csel encodes CSEL rd, rn, rm, cond.
func Csel(w64 bool, rd, rn, rm Reg, c Cond) uint32 {
	return sf(w64)<<31 | 0xd4<<21 | r(rm)<<16 | uint32(c)<<12 | r(rn)<<5 | r(rd)
}

// Cset encodes CSET rd, cond (rd = 1 if cond else 0) via CSINC rd, ZR, ZR, ~cond.
func Cset(w64 bool, rd Reg, c Cond) uint32 {
	return sf(w64)<<31 | 0xd4<<21 | r(ZR)<<16 | uint32(c.invert())<<12 | 1<<10 | r(ZR)<<5 | r(rd)
}

// CcmpReg encodes CCMP rn, rm, #nzcv, cond: if cond holds, compare rn,rm and set
// the flags; otherwise set the flags to nzcv. It chains comparisons for a
// conjoined condition without a branch between them.
func CcmpReg(w64 bool, rn, rm Reg, nzcv uint32, c Cond) uint32 {
	return sf(w64)<<31 | 1<<30 | 1<<29 | 0xd2<<21 | r(rm)<<16 | uint32(c)<<12 | r(rn)<<5 | field(nzcv, 4, "ccmp nzcv")
}

// CcmpImm encodes CCMP rn, #imm5, #nzcv, cond (imm5 an unsigned 0..31).
func CcmpImm(w64 bool, rn Reg, imm5, nzcv uint32, c Cond) uint32 {
	return sf(w64)<<31 | 1<<30 | 1<<29 | 0xd2<<21 | field(imm5, 5, "ccmp imm")<<16 | uint32(c)<<12 | 1<<11 | r(rn)<<5 | field(nzcv, 4, "ccmp nzcv")
}

// FailNZCV returns an NZCV flag value (bit3=N,2=Z,1=C,0=V) under which condition
// c evaluates false -- the value a CCMP feeds forward so a conjoined test folds:
// for `a && b`, CCMP on cond(a) forwards FailNZCV(cond(b)) so the final branch on
// cond(b) is not taken when a failed. PassNZCV(c) = FailNZCV(c.invert()).
func FailNZCV(c Cond) uint32 {
	switch c {
	case EQ, CS, MI, VS, LT, LE:
		return 0b0000
	case NE, HI, GT:
		return 0b0100 // Z=1
	case CC, LS:
		return 0b0010 // C=1
	case PL, GE:
		return 0b1000 // N=1
	case VC:
		return 0b0001 // V=1
	}
	return 0b0000
}

// PassNZCV returns an NZCV value under which condition c evaluates true, for the
// `a || b` fold: CCMP on cond(a).invert() forwards PassNZCV(cond(b)) so the final
// branch is taken when a already succeeded.
func PassNZCV(c Cond) uint32 { return FailNZCV(c.invert()) }

// --- branches --------------------------------------------------------------

func branchImm(link uint32, off int32) uint32 {
	return 0x05<<26 | link<<31 | branchOff(off, 26, "b/bl")
}

// B encodes B to a PC-relative byte offset (a multiple of 4).
func B(off int32) uint32 { return branchImm(0, off) }

// Bl encodes BL to a PC-relative byte offset.
func Bl(off int32) uint32 { return branchImm(1, off) }

// Bcond encodes B.cond to a PC-relative byte offset.
func Bcond(c Cond, off int32) uint32 {
	return 0x54<<24 | branchOff(off, 19, "b.cond")<<5 | uint32(c)
}

func cmpBranch(w64 bool, nz uint32, rt Reg, off int32) uint32 {
	return sf(w64)<<31 | 0x1a<<25 | nz<<24 | branchOff(off, 19, "cbz/cbnz")<<5 | r(rt)
}

// Cbz encodes CBZ rt, offset.
func Cbz(w64 bool, rt Reg, off int32) uint32 { return cmpBranch(w64, 0, rt, off) }

// Cbnz encodes CBNZ rt, offset.
func Cbnz(w64 bool, rt Reg, off int32) uint32 { return cmpBranch(w64, 1, rt, off) }

// testBranch encodes TBZ/TBNZ: branch on bit `bit` (0..63) of rt being zero (op=0)
// or non-zero (op=1). The offset is a 14-bit signed byte offset, so the reach is
// only +/-32 KiB -- callers must ensure the target is within range.
func testBranch(op, bit uint32, rt Reg, off int32) uint32 {
	return (bit>>5)<<31 | 0x1b<<25 | op<<24 | (bit&0x1f)<<19 | branchOff(off, 14, "tbz/tbnz")<<5 | r(rt)
}

// Tbz encodes TBZ rt, #bit, offset (branch if bit clear).
func Tbz(bit uint32, rt Reg, off int32) uint32 { return testBranch(0, bit, rt, off) }

// Tbnz encodes TBNZ rt, #bit, offset (branch if bit set).
func Tbnz(bit uint32, rt Reg, off int32) uint32 { return testBranch(1, bit, rt, off) }

// Ret encodes RET rn (defaults to the link register when rn is X30).
func Ret(rn Reg) uint32 { return 0xd65f0000 | r(rn)<<5 }

// Br encodes BR rn (indirect branch).
func Br(rn Reg) uint32 { return 0xd61f0000 | r(rn)<<5 }

// Blr encodes BLR rn (indirect call).
func Blr(rn Reg) uint32 { return 0xd63f0000 | r(rn)<<5 }

// Brk encodes BRK #imm16 (a breakpoint / trap).
func Brk(imm16 uint16) uint32 { return 0xd4200000 | uint32(imm16)<<5 }

// --- loads and stores (unsigned offset) ------------------------------------

// size selects the access width: 0=byte, 1=halfword, 2=word(32), 3=doubleword(64).
func ldStr(size, opc uint32, rt, rn Reg, imm12 uint32) uint32 {
	return size<<30 | 0x39<<24 | opc<<22 | field(imm12, 12, "ldr/str")<<10 | r(rn)<<5 | r(rt)
}

// StrImm encodes STR rt, [rn, #imm]; imm is a byte offset, scaled by the width.
func StrImm(w64 bool, rt, rn Reg, imm uint32) uint32 {
	if w64 {
		return ldStr(3, 0, rt, rn, scaled(imm, 8, "str"))
	}
	return ldStr(2, 0, rt, rn, scaled(imm, 4, "str"))
}

// LdrImm encodes LDR rt, [rn, #imm].
func LdrImm(w64 bool, rt, rn Reg, imm uint32) uint32 {
	if w64 {
		return ldStr(3, 1, rt, rn, scaled(imm, 8, "ldr"))
	}
	return ldStr(2, 1, rt, rn, scaled(imm, 4, "ldr"))
}

// StrbImm / StrhImm store the low byte / halfword; LdrbImm / LdrhImm load and
// zero-extend. imm is an unscaled byte offset (byte) or /2 (halfword).
func StrbImm(rt, rn Reg, imm uint32) uint32 { return ldStr(0, 0, rt, rn, imm) }
func LdrbImm(rt, rn Reg, imm uint32) uint32 { return ldStr(0, 1, rt, rn, imm) }
func StrhImm(rt, rn Reg, imm uint32) uint32 { return ldStr(1, 0, rt, rn, scaled(imm, 2, "strh")) }
func LdrhImm(rt, rn Reg, imm uint32) uint32 { return ldStr(1, 1, rt, rn, scaled(imm, 2, "ldrh")) }

// LdrswImm encodes LDRSW rt, [rn, #imm] (load word, sign-extend to 64 bits).
func LdrswImm(rt, rn Reg, imm uint32) uint32 { return ldStr(2, 2, rt, rn, scaled(imm, 4, "ldrsw")) }

// Extend options for a register-offset load/store: how the index register Rm is
// widened to an address. LSL uses a 64-bit index directly; SXTW/UXTW sign- or
// zero-extend a 32-bit index.
const (
	ExtUXTW uint32 = 0b010
	ExtLSL  uint32 = 0b011 // UXTX for an X register: no extend
	ExtSXTW uint32 = 0b110
	ExtSXTX uint32 = 0b111
)

// ldStrReg encodes a load/store with a register offset: [Rn, Rm, <extend> #S*log2(size)].
// S=1 scales the index by the access width, S=0 leaves it unscaled.
func ldStrReg(size, opc uint32, rt, rn, rm Reg, option, s uint32) uint32 {
	return size<<30 | 0x38<<24 | opc<<22 | 1<<21 | r(rm)<<16 | field(option, 3, "ext option")<<13 | field(s, 1, "ext scale")<<12 | 0b10<<10 | r(rn)<<5 | r(rt)
}

func regSize(w64 bool) uint32 {
	if w64 {
		return 3
	}
	return 2
}

// LdrReg / StrReg encode a word/doubleword load/store with a register offset.
func LdrReg(w64 bool, rt, rn, rm Reg, option, s uint32) uint32 {
	return ldStrReg(regSize(w64), 1, rt, rn, rm, option, s)
}
func StrReg(w64 bool, rt, rn, rm Reg, option, s uint32) uint32 {
	return ldStrReg(regSize(w64), 0, rt, rn, rm, option, s)
}

// LdrbReg / StrbReg / LdrhReg / StrhReg: byte and halfword register-offset forms.
func LdrbReg(rt, rn, rm Reg, option, s uint32) uint32 { return ldStrReg(0, 1, rt, rn, rm, option, s) }
func StrbReg(rt, rn, rm Reg, option, s uint32) uint32 { return ldStrReg(0, 0, rt, rn, rm, option, s) }
func LdrhReg(rt, rn, rm Reg, option, s uint32) uint32 { return ldStrReg(1, 1, rt, rn, rm, option, s) }
func StrhReg(rt, rn, rm Reg, option, s uint32) uint32 { return ldStrReg(1, 0, rt, rn, rm, option, s) }

// Sign-extending register-offset loads.
func LdrswReg(rt, rn, rm Reg, option, s uint32) uint32 { return ldStrReg(2, 2, rt, rn, rm, option, s) }
func LdrsbReg(w64 bool, rt, rn, rm Reg, option, s uint32) uint32 {
	return ldStrReg(0, signedLoadOpc(w64), rt, rn, rm, option, s)
}
func LdrshReg(w64 bool, rt, rn, rm Reg, option, s uint32) uint32 {
	return ldStrReg(1, signedLoadOpc(w64), rt, rn, rm, option, s)
}

// --- address, shifted immediate, bitfield, neg -----------------------------

// Adrp encodes ADRP rd, <page>: the PC-relative page address. imm is the signed
// 21-bit page offset; pass 0 with an ADR_PREL_PG_HI21 relocation for a symbol.
func Adrp(rd Reg, imm int32) uint32 {
	u := sfield(imm, 21, "adrp")
	return 1<<31 | (u&3)<<29 | 0x10<<24 | (u>>2)<<5 | r(rd)
}

// Adr encodes ADR rd, <label>: the PC-relative byte address. imm is the signed
// 21-bit byte offset from the instruction (±1 MiB).
func Adr(rd Reg, imm int32) uint32 {
	u := sfield(imm, 21, "adr")
	return (u&3)<<29 | 0x10<<24 | (u>>2)<<5 | r(rd)
}

// AddImmLSL12 / SubImmLSL12 encode ADD/SUB rd, rn, #imm12, LSL #12.
func AddImmLSL12(w64 bool, rd, rn Reg, imm12 uint32) uint32 {
	return addSubImm(w64, 0, 0, rd, rn, imm12) | 1<<22
}
func SubImmLSL12(w64 bool, rd, rn Reg, imm12 uint32) uint32 {
	return addSubImm(w64, 1, 0, rd, rn, imm12) | 1<<22
}

func bitfield(w64 bool, opc uint32, rd, rn Reg, immr, imms uint32) uint32 {
	return sf(w64)<<31 | opc<<29 | 0x26<<23 | sf(w64)<<22 | field(immr, 6, "bitfield immr")<<16 | field(imms, 6, "bitfield imms")<<10 | r(rn)<<5 | r(rd)
}

// Sbfm / Ubfm encode the signed/unsigned bitfield-move instructions the extends
// and immediate shifts alias.
func Sbfm(w64 bool, rd, rn Reg, immr, imms uint32) uint32 {
	return bitfield(w64, 0, rd, rn, immr, imms)
}
func Ubfm(w64 bool, rd, rn Reg, immr, imms uint32) uint32 {
	return bitfield(w64, 2, rd, rn, immr, imms)
}

// Sign/zero extends. Sxtb/Sxth take a destination width; Sxtw is always X, and
// Uxtb/Uxth are always W (the zero-extend fills the 64-bit register anyway).
func Sxtb(w64 bool, rd, rn Reg) uint32 { return Sbfm(w64, rd, rn, 0, 7) }
func Sxth(w64 bool, rd, rn Reg) uint32 { return Sbfm(w64, rd, rn, 0, 15) }
func Sxtw(rd, rn Reg) uint32           { return Sbfm(true, rd, rn, 0, 31) }
func Uxtb(rd, rn Reg) uint32           { return Ubfm(false, rd, rn, 0, 7) }
func Uxth(rd, rn Reg) uint32           { return Ubfm(false, rd, rn, 0, 15) }

// NegReg encodes NEG rd, rm (SUB rd, ZR, rm).
func NegReg(w64 bool, rd, rm Reg) uint32 { return SubReg(w64, rd, ZR, rm) }

// --- immediate forms: cmp, shifts, logical, rotate ------------------------

// SubsImm encodes SUBS rd, rn, #imm12 (sets flags).
func SubsImm(w64 bool, rd, rn Reg, imm12 uint32) uint32 { return addSubImm(w64, 1, 1, rd, rn, imm12) }

// CmpImm encodes CMP rn, #imm12 (SUBS ZR, rn, #imm12).
func CmpImm(w64 bool, rn Reg, imm12 uint32) uint32 { return SubsImm(w64, ZR, rn, imm12) }

// LslImm encodes LSL rd, rn, #sh (an alias of UBFM). sh is 0..width-1.
func LslImm(w64 bool, rd, rn Reg, sh uint32) uint32 {
	width := uint32(32)
	if w64 {
		width = 64
	}
	return Ubfm(w64, rd, rn, (width-sh)&(width-1), width-1-sh)
}

// LsrImm encodes LSR rd, rn, #sh (an alias of UBFM).
func LsrImm(w64 bool, rd, rn Reg, sh uint32) uint32 {
	width := uint32(32)
	if w64 {
		width = 64
	}
	return Ubfm(w64, rd, rn, sh, width-1)
}

// AsrImm encodes ASR rd, rn, #sh (an alias of SBFM).
func AsrImm(w64 bool, rd, rn Reg, sh uint32) uint32 {
	width := uint32(32)
	if w64 {
		width = 64
	}
	return Sbfm(w64, rd, rn, sh, width-1)
}

func logicalImm(w64 bool, opc uint32, rd, rn Reg, n, immr, imms uint32) uint32 {
	return sf(w64)<<31 | opc<<29 | 0x24<<23 | field(n, 1, "logical N")<<22 | field(immr, 6, "logical immr")<<16 | field(imms, 6, "logical imms")<<10 | r(rn)<<5 | r(rd)
}

// AndImm/OrrImm/EorImm encode the logical-immediate instructions; the caller
// supplies the (N, immr, imms) bitmask fields from EncodeBitmask.
func AndImm(w64 bool, rd, rn Reg, n, immr, imms uint32) uint32 {
	return logicalImm(w64, 0, rd, rn, n, immr, imms)
}
func OrrImm(w64 bool, rd, rn Reg, n, immr, imms uint32) uint32 {
	return logicalImm(w64, 1, rd, rn, n, immr, imms)
}
func EorImm(w64 bool, rd, rn Reg, n, immr, imms uint32) uint32 {
	return logicalImm(w64, 2, rd, rn, n, immr, imms)
}

// AndsImm/AndsReg encode ANDS (AND setting the flags). With rd = XZR (31) this is
// TST -- a flags-only mask test, the `if (x & mask)` branch condition.
func AndsImm(w64 bool, rd, rn Reg, n, immr, imms uint32) uint32 {
	return logicalImm(w64, 3, rd, rn, n, immr, imms)
}
func AndsReg(w64 bool, rd, rn, rm Reg) uint32 { return logicalReg(w64, 3, rd, rn, rm) }

// Extr encodes EXTR rd, rn, rm, #lsb (the low bits are taken from rm, the high
// bits from rn). ROR is EXTR rd, rn, rn, #shift.
func Extr(w64 bool, rd, rn, rm Reg, lsb uint32) uint32 {
	return sf(w64)<<31 | 0x27<<23 | sf(w64)<<22 | r(rm)<<16 | field(lsb, 6, "extr lsb")<<10 | r(rn)<<5 | r(rd)
}

// RorImm encodes ROR rd, rn, #sh (EXTR rd, rn, rn, #sh) — a rotate right.
func RorImm(w64 bool, rd, rn Reg, sh uint32) uint32 { return Extr(w64, rd, rn, rn, sh) }

// EncodeBitmask returns the (N, immr, imms) fields for val as an AArch64 logical
// (bitmask) immediate over a regSize-byte register, or ok=false if val is not a
// representable bitmask (all-zeros, all-ones, and non-repeating patterns are
// not). The search is over the definition of the encoding — every element size,
// run length, and rotation — so it is correct by construction.
func EncodeBitmask(val uint64, regSize int) (n, immr, imms uint32, ok bool) {
	bits := regSize * 8
	if bits < 64 {
		val &= (uint64(1) << uint(bits)) - 1
	}
	var full uint64 = ^uint64(0)
	if bits < 64 {
		full = (uint64(1) << uint(bits)) - 1
	}
	if val == 0 || val == full {
		return 0, 0, 0, false // all-zeros / all-ones are not encodable
	}
	for esize := 2; esize <= bits; esize <<= 1 {
		emask := (uint64(1) << uint(esize)) - 1
		for nones := 1; nones < esize; nones++ {
			base := (uint64(1) << uint(nones)) - 1 // nones ones at the bottom
			for rot := 0; rot < esize; rot++ {
				elem := ((base >> uint(rot)) | (base << uint(esize-rot))) & emask
				// Replicate the element across the whole register.
				v := elem
				for w := esize; w < bits; w <<= 1 {
					v |= v << uint(w)
				}
				if bits < 64 {
					v &= full
				}
				if v != val {
					continue
				}
				log2e := 0
				for (1 << uint(log2e)) < esize {
					log2e++
				}
				if esize == 64 {
					return 1, uint32(rot), uint32(nones - 1), true
				}
				high := uint32((1<<uint(6-log2e))-2) << uint(log2e)
				return 0, uint32(rot), high | uint32(nones-1), true
			}
		}
	}
	return 0, 0, 0, false
}
