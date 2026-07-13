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

func r(x Reg) uint32 { return uint32(x) & 0x1f }

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

// SubsReg encodes SUBS rd, rn, rm (sets flags); CmpReg is SUBS ZR, rn, rm.
func SubsReg(w64 bool, rd, rn, rm Reg) uint32 { return addSubReg(w64, 1, 1, rd, rn, rm) }

// CmpReg encodes CMP rn, rm.
func CmpReg(w64 bool, rn, rm Reg) uint32 { return SubsReg(w64, ZR, rn, rm) }

func addSubImm(w64 bool, op, s uint32, rd, rn Reg, imm12 uint32) uint32 {
	return sf(w64)<<31 | op<<30 | s<<29 | 0x11<<24 | (imm12&0xfff)<<10 | r(rn)<<5 | r(rd)
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

// --- move wide immediate ---------------------------------------------------

func moveWide(w64 bool, opc uint32, rd Reg, imm16 uint16, shift uint32) uint32 {
	hw := (shift / 16) & 3
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

// Csel encodes CSEL rd, rn, rm, cond.
func Csel(w64 bool, rd, rn, rm Reg, c Cond) uint32 {
	return sf(w64)<<31 | 0xd4<<21 | r(rm)<<16 | uint32(c)<<12 | r(rn)<<5 | r(rd)
}

// Cset encodes CSET rd, cond (rd = 1 if cond else 0) via CSINC rd, ZR, ZR, ~cond.
func Cset(w64 bool, rd Reg, c Cond) uint32 {
	return sf(w64)<<31 | 0xd4<<21 | r(ZR)<<16 | uint32(c.invert())<<12 | 1<<10 | r(ZR)<<5 | r(rd)
}

// --- branches --------------------------------------------------------------

func branchImm(link uint32, off int32) uint32 {
	return 0x05<<26 | link<<31 | uint32(off/4)&0x03ffffff
}

// B encodes B to a PC-relative byte offset (a multiple of 4).
func B(off int32) uint32 { return branchImm(0, off) }

// Bl encodes BL to a PC-relative byte offset.
func Bl(off int32) uint32 { return branchImm(1, off) }

// Bcond encodes B.cond to a PC-relative byte offset.
func Bcond(c Cond, off int32) uint32 {
	return 0x54<<24 | (uint32(off/4)&0x7ffff)<<5 | uint32(c)
}

func cmpBranch(w64 bool, nz uint32, rt Reg, off int32) uint32 {
	return sf(w64)<<31 | 0x1a<<25 | nz<<24 | (uint32(off/4)&0x7ffff)<<5 | r(rt)
}

// Cbz encodes CBZ rt, offset.
func Cbz(w64 bool, rt Reg, off int32) uint32 { return cmpBranch(w64, 0, rt, off) }

// Cbnz encodes CBNZ rt, offset.
func Cbnz(w64 bool, rt Reg, off int32) uint32 { return cmpBranch(w64, 1, rt, off) }

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
	return size<<30 | 0x39<<24 | opc<<22 | (imm12&0xfff)<<10 | r(rn)<<5 | r(rt)
}

// StrImm encodes STR rt, [rn, #imm]; imm is a byte offset, scaled by the width.
func StrImm(w64 bool, rt, rn Reg, imm uint32) uint32 {
	if w64 {
		return ldStr(3, 0, rt, rn, imm/8)
	}
	return ldStr(2, 0, rt, rn, imm/4)
}

// LdrImm encodes LDR rt, [rn, #imm].
func LdrImm(w64 bool, rt, rn Reg, imm uint32) uint32 {
	if w64 {
		return ldStr(3, 1, rt, rn, imm/8)
	}
	return ldStr(2, 1, rt, rn, imm/4)
}

// StrbImm / StrhImm store the low byte / halfword; LdrbImm / LdrhImm load and
// zero-extend. imm is an unscaled byte offset (byte) or /2 (halfword).
func StrbImm(rt, rn Reg, imm uint32) uint32 { return ldStr(0, 0, rt, rn, imm) }
func LdrbImm(rt, rn Reg, imm uint32) uint32 { return ldStr(0, 1, rt, rn, imm) }
func StrhImm(rt, rn Reg, imm uint32) uint32 { return ldStr(1, 0, rt, rn, imm/2) }
func LdrhImm(rt, rn Reg, imm uint32) uint32 { return ldStr(1, 1, rt, rn, imm/2) }

// LdrswImm encodes LDRSW rt, [rn, #imm] (load word, sign-extend to 64 bits).
func LdrswImm(rt, rn Reg, imm uint32) uint32 { return ldStr(2, 2, rt, rn, imm/4) }

// --- address, shifted immediate, bitfield, neg -----------------------------

// Adrp encodes ADRP rd, <page>: the PC-relative page address. imm is the signed
// 21-bit page offset; pass 0 with an ADR_PREL_PG_HI21 relocation for a symbol.
func Adrp(rd Reg, imm int32) uint32 {
	u := uint32(imm) & 0x1fffff
	return 1<<31 | (u&3)<<29 | 0x10<<24 | (u>>2)<<5 | r(rd)
}

// AddImmLSL12 / SubImmLSL12 encode ADD/SUB rd, rn, #imm12, LSL #12.
func AddImmLSL12(w64 bool, rd, rn Reg, imm12 uint32) uint32 {
	return addSubImm(w64, 0, 0, rd, rn, imm12) | 1<<22
}
func SubImmLSL12(w64 bool, rd, rn Reg, imm12 uint32) uint32 {
	return addSubImm(w64, 1, 0, rd, rn, imm12) | 1<<22
}

func bitfield(w64 bool, opc uint32, rd, rn Reg, immr, imms uint32) uint32 {
	return sf(w64)<<31 | opc<<29 | 0x26<<23 | sf(w64)<<22 | (immr&0x3f)<<16 | (imms&0x3f)<<10 | r(rn)<<5 | r(rd)
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
