package a64

import "fmt"

// Additional loads and stores: register pairs, signed sub-word loads, and
// floating-point loads/stores.

// PairMode is the addressing mode of a load/store pair.
type PairMode uint32

const (
	SignedOffset PairMode = 2 // [rn, #imm]
	PostIndex    PairMode = 1 // [rn], #imm   (writeback after)
	PreIndex     PairMode = 3 // [rn, #imm]!  (writeback before)
)

func ldStPair(w64 bool, load uint32, mode PairMode, rt, rt2, rn Reg, imm int) uint32 {
	opc, scale := uint32(0), 4
	if w64 {
		opc, scale = 2, 8
	}
	if imm%scale != 0 {
		panic(fmt.Sprintf("a64: ldp/stp offset %d is not a multiple of %d", imm, scale))
	}
	imm7 := sfield(int32(imm/scale), 7, "ldp/stp")
	return opc<<30 | 0b101<<27 | uint32(mode)<<23 | load<<22 | imm7<<15 | r(rt2)<<10 | r(rn)<<5 | r(rt)
}

// Stp encodes STP rt, rt2, [rn ...] with the given addressing mode.
func Stp(w64 bool, rt, rt2, rn Reg, imm int, mode PairMode) uint32 {
	return ldStPair(w64, 0, mode, rt, rt2, rn, imm)
}

// Ldp encodes LDP rt, rt2, [rn ...].
func Ldp(w64 bool, rt, rt2, rn Reg, imm int, mode PairMode) uint32 {
	return ldStPair(w64, 1, mode, rt, rt2, rn, imm)
}

// signedLoadOpc is opc for a sign-extending load: 10 for an X destination, 11
// for a W destination.
func signedLoadOpc(w64 bool) uint32 {
	if w64 {
		return 2
	}
	return 3
}

// LdrsbImm / LdrshImm load a byte/halfword and sign-extend into rt.
func LdrsbImm(w64 bool, rt, rn Reg, imm uint32) uint32 {
	return ldStr(0, signedLoadOpc(w64), rt, rn, imm)
}
func LdrshImm(w64 bool, rt, rn Reg, imm uint32) uint32 {
	return ldStr(1, signedLoadOpc(w64), rt, rn, imm/2)
}

// Pre/post-indexed single-register loads and stores (write-back addressing):
// `ldr rt, [rn], #imm` (post) accesses [rn] then sets rn = rn + imm; `ldr rt,
// [rn, #imm]!` (pre) sets rn = rn + imm then accesses [rn]. Unlike the
// unsigned-offset forms, imm is an UNSCALED signed byte offset in [-256, 255] and
// the base register rn is updated as a side effect.
type WriteBack uint32

const (
	Post WriteBack = 1 // [rn], #imm  -- access, then rn += imm
	Pre  WriteBack = 3 // [rn, #imm]! -- rn += imm, then access
)

func ldStrIdx(size, opc uint32, rt, rn Reg, imm int32, wb WriteBack) uint32 {
	if imm < -256 || imm > 255 {
		panic(fmt.Sprintf("a64: write-back offset %d out of range [-256,255]", imm))
	}
	return size<<30 | 0x38<<24 | opc<<22 | (uint32(imm)&0x1ff)<<12 | uint32(wb)<<10 | r(rn)<<5 | r(rt)
}

func LdrWB(w64 bool, rt, rn Reg, imm int32, wb WriteBack) uint32 {
	if w64 {
		return ldStrIdx(3, 1, rt, rn, imm, wb)
	}
	return ldStrIdx(2, 1, rt, rn, imm, wb)
}
func StrWB(w64 bool, rt, rn Reg, imm int32, wb WriteBack) uint32 {
	if w64 {
		return ldStrIdx(3, 0, rt, rn, imm, wb)
	}
	return ldStrIdx(2, 0, rt, rn, imm, wb)
}
func LdrbWB(rt, rn Reg, imm int32, wb WriteBack) uint32 { return ldStrIdx(0, 1, rt, rn, imm, wb) }
func StrbWB(rt, rn Reg, imm int32, wb WriteBack) uint32 { return ldStrIdx(0, 0, rt, rn, imm, wb) }
func LdrhWB(rt, rn Reg, imm int32, wb WriteBack) uint32 { return ldStrIdx(1, 1, rt, rn, imm, wb) }
func StrhWB(rt, rn Reg, imm int32, wb WriteBack) uint32 { return ldStrIdx(1, 0, rt, rn, imm, wb) }
func LdrsbWB(w64 bool, rt, rn Reg, imm int32, wb WriteBack) uint32 {
	return ldStrIdx(0, signedLoadOpc(w64), rt, rn, imm, wb)
}
func LdrshWB(w64 bool, rt, rn Reg, imm int32, wb WriteBack) uint32 {
	return ldStrIdx(1, signedLoadOpc(w64), rt, rn, imm, wb)
}
func LdrswWB(rt, rn Reg, imm int32, wb WriteBack) uint32 { return ldStrIdx(2, 2, rt, rn, imm, wb) }

// FP loads and stores (unsigned offset). The size field selects S (word) or D
// (doubleword); V=1 (bit in the fixed 0x3d field) marks the SIMD/FP variant.
func ldStrFP(size, opc uint32, rt, rn Reg, imm uint32) uint32 {
	return size<<30 | 0x3d<<24 | opc<<22 | field(imm, 12, "fp ldr/str")<<10 | r(rn)<<5 | r(rt)
}

// StrFP / LdrFP store/load an S or D register. imm is a byte offset scaled by
// the access width.
func StrFP(dbl bool, rt, rn Reg, imm uint32) uint32 {
	if dbl {
		return ldStrFP(3, 0, rt, rn, imm/8)
	}
	return ldStrFP(2, 0, rt, rn, imm/4)
}
func LdrFP(dbl bool, rt, rn Reg, imm uint32) uint32 {
	if dbl {
		return ldStrFP(3, 1, rt, rn, imm/8)
	}
	return ldStrFP(2, 1, rt, rn, imm/4)
}

// StrQ / LdrQ store/load a 128-bit Q register (size=00, opc=10/11). imm is a
// byte offset scaled by 16.
func StrQ(rt, rn Reg, imm uint32) uint32 { return ldStrFP(0, 0b10, rt, rn, imm/16) }
func LdrQ(rt, rn Reg, imm uint32) uint32 { return ldStrFP(0, 0b11, rt, rn, imm/16) }
