package a64

import (
	"fmt"
	"strings"
)

// Disasm renders one A64 instruction word as assembly text. It is the inverse of
// the encoders in this package, and exists so that tests and tools can read the
// machine code we emit without a second, independently-written text emitter to
// keep in step. A word this package does not know how to encode comes back as a
// `.inst` directive rather than a guess.
//
// Only what the encoders produce is decoded. Targets are PC-relative and printed
// as offsets from the instruction; naming them needs relocations, which live a
// layer up.
func Disasm(w uint32) string {
	for _, try := range []func(uint32) string{
		disasmBranch,
		disasmDPImm,
		disasmDPReg,
		disasmLdSt,
		disasmFP,
		disasmSystem,
	} {
		if s := try(w); s != "" {
			return s
		}
	}
	return fmt.Sprintf(".inst %#08x", w)
}

// --- text helpers ----------------------------------------------------------

// gpr names a general register in the zero-register context: 31 reads as zero
// rather than as the stack pointer.
func gpr(w64 bool, x uint32) string {
	if x == 31 {
		if w64 {
			return "xzr"
		}
		return "wzr"
	}
	return fmt.Sprintf("%c%d", regLetter(w64), x)
}

// gprSP names a general register where 31 means the stack pointer: the base of a
// memory operand, and the operands of add/sub immediate.
func gprSP(w64 bool, x uint32) string {
	if x == 31 {
		if w64 {
			return "sp"
		}
		return "wsp"
	}
	return fmt.Sprintf("%c%d", regLetter(w64), x)
}

func regLetter(w64 bool) byte {
	if w64 {
		return 'x'
	}
	return 'w'
}

// fpr names a scalar FP register of the width the ftype field selects.
func fpr(dbl bool, x uint32) string {
	if dbl {
		return fmt.Sprintf("d%d", x)
	}
	return fmt.Sprintf("s%d", x)
}

var condNames = [16]string{
	"eq", "ne", "cs", "cc", "mi", "pl", "vs", "vc",
	"hi", "ls", "ge", "lt", "gt", "le", "al", "nv",
}

// extendNames are the register-extend options, indexed by the 3-bit option field.
var extendNames = [8]string{"uxtb", "uxth", "uxtw", "uxtx", "sxtb", "sxth", "sxtw", "sxtx"}

// shiftNames are the shift kinds of a shifted-register operand.
var shiftNames = [4]string{"lsl", "lsr", "asr", "ror"}

// sext sign-extends the low `bits` of v.
func sext(v uint32, bits uint) int32 { return int32(v<<(32-bits)) >> (32 - bits) }

// pcrel renders a PC-relative byte offset the way it is written in source: `.`
// for the instruction itself, `.+8` / `.-4` otherwise.
func pcrel(off int32) string {
	if off == 0 {
		return "."
	}
	return fmt.Sprintf(".%+d", off)
}

// bit reports whether bit n of w is set.
func bit(w uint32, n uint) bool { return w>>n&1 == 1 }

// bits extracts the field from bit hi down to bit lo, inclusive.
func bits(w uint32, hi, lo uint) uint32 { return w >> lo & (1<<(hi-lo+1) - 1) }

// widthOf is the register width in bits that the sf flag selects.
func widthOf(w64 bool) uint32 {
	if w64 {
		return 64
	}
	return 32
}

// --- branches and exceptions -----------------------------------------------

func disasmBranch(w uint32) string {
	switch {
	case w&0x7c000000 == 0x14000000: // B / BL
		op := "b"
		if bit(w, 31) {
			op = "bl"
		}
		return fmt.Sprintf("%s %s", op, pcrel(sext(bits(w, 25, 0), 26)*4))

	case w&0xff000010 == 0x54000000: // B.cond
		return fmt.Sprintf("b.%s %s", condNames[w&0xf], pcrel(sext(bits(w, 23, 5), 19)*4))

	case w&0x7e000000 == 0x34000000: // CBZ / CBNZ
		op := "cbz"
		if bit(w, 24) {
			op = "cbnz"
		}
		return fmt.Sprintf("%s %s, %s", op, gpr(bit(w, 31), w&0x1f), pcrel(sext(bits(w, 23, 5), 19)*4))

	case w&^uint32(0x3e0) == 0xd65f0000: // RET
		if rn := bits(w, 9, 5); rn != 30 {
			return fmt.Sprintf("ret x%d", rn)
		}
		return "ret"

	case w&^uint32(0x3e0) == 0xd61f0000: // BR
		return fmt.Sprintf("br x%d", bits(w, 9, 5))

	case w&^uint32(0x3e0) == 0xd63f0000: // BLR
		return fmt.Sprintf("blr x%d", bits(w, 9, 5))

	case w&^uint32(0x1fffe0) == 0xd4200000: // BRK
		return fmt.Sprintf("brk #%d", bits(w, 20, 5))
	}
	return ""
}

// --- data processing: immediate --------------------------------------------

func disasmDPImm(w uint32) string {
	if bits(w, 28, 26) != 0b100 {
		return ""
	}
	switch bits(w, 28, 23) {
	case 0x20, 0x21: // ADR / ADRP
		imm := sext(bits(w, 23, 5)<<2|bits(w, 30, 29), 21)
		if bit(w, 31) {
			return fmt.Sprintf("adrp x%d, %s", w&0x1f, pcrel(imm*4096))
		}
		return fmt.Sprintf("adr x%d, %s", w&0x1f, pcrel(imm))

	case 0x22, 0x23: // ADD/SUB (immediate)
		return disasmAddSubImm(w)

	case 0x24: // logical (immediate)
		return disasmLogicalImm(w)

	case 0x25: // MOVZ / MOVK / MOVN
		return disasmMoveWide(w)

	case 0x26: // bitfield
		return disasmBitfield(w)

	case 0x27: // EXTR
		w64 := bit(w, 31)
		rd, rn, rm, lsb := w&0x1f, bits(w, 9, 5), bits(w, 20, 16), bits(w, 15, 10)
		if rn == rm { // ROR is the same-source EXTR
			return fmt.Sprintf("ror %s, %s, #%d", gpr(w64, rd), gpr(w64, rn), lsb)
		}
		return fmt.Sprintf("extr %s, %s, %s, #%d", gpr(w64, rd), gpr(w64, rn), gpr(w64, rm), lsb)
	}
	return ""
}

func disasmAddSubImm(w uint32) string {
	w64, rd, rn, imm := bit(w, 31), w&0x1f, bits(w, 9, 5), bits(w, 21, 10)
	op := "add"
	if bit(w, 30) {
		op = "sub"
	}
	shift := ""
	if bit(w, 22) {
		shift = ", lsl #12"
	}
	if bit(w, 29) { // S: sets flags
		op += "s"
		if rd == 31 { // discarding the result is a compare
			return fmt.Sprintf("cmp %s, #%d%s", gprSP(w64, rn), imm, shift)
		}
	}
	return fmt.Sprintf("%s %s, %s, #%d%s", op, gprSP(w64, rd), gprSP(w64, rn), imm, shift)
}

func disasmLogicalImm(w uint32) string {
	w64, rd, rn := bit(w, 31), w&0x1f, bits(w, 9, 5)
	val, ok := decodeBitmask(bits(w, 22, 22), bits(w, 21, 16), bits(w, 15, 10), int(widthOf(w64)))
	if !ok {
		return ""
	}
	op := [4]string{"and", "orr", "eor", "ands"}[bits(w, 30, 29)]
	if op == "ands" && rd == 31 {
		return fmt.Sprintf("tst %s, #%#x", gpr(w64, rn), val)
	}
	if op == "orr" && rn == 31 {
		return fmt.Sprintf("mov %s, #%#x", gprSP(w64, rd), val)
	}
	dst := gprSP(w64, rd) // the logical immediates write the SP form
	if op == "ands" {
		dst = gpr(w64, rd)
	}
	return fmt.Sprintf("%s %s, %s, #%#x", op, dst, gpr(w64, rn), val)
}

func disasmMoveWide(w uint32) string {
	op, ok := map[uint32]string{0: "movn", 2: "movz", 3: "movk"}[bits(w, 30, 29)]
	if !ok {
		return ""
	}
	shift := ""
	if hw := bits(w, 22, 21); hw != 0 {
		shift = fmt.Sprintf(", lsl #%d", hw*16)
	}
	return fmt.Sprintf("%s %s, #%#x%s", op, gpr(bit(w, 31), w&0x1f), bits(w, 20, 5), shift)
}

func disasmBitfield(w uint32) string {
	w64, rd, rn := bit(w, 31), w&0x1f, bits(w, 9, 5)
	immr, imms, width := bits(w, 21, 16), bits(w, 15, 10), widthOf(w64)
	d, s := gpr(w64, rd), gpr(w64, rn)

	switch bits(w, 30, 29) {
	case 0: // SBFM: the arithmetic shift and the sign extends alias it
		switch {
		case imms == width-1:
			return fmt.Sprintf("asr %s, %s, #%d", d, s, immr)
		case immr == 0 && imms == 7:
			return fmt.Sprintf("sxtb %s, %s", d, gpr(false, rn))
		case immr == 0 && imms == 15:
			return fmt.Sprintf("sxth %s, %s", d, gpr(false, rn))
		case immr == 0 && imms == 31 && w64:
			return fmt.Sprintf("sxtw %s, %s", d, gpr(false, rn))
		}
		return fmt.Sprintf("sbfx %s, %s, #%d, #%d", d, s, immr, imms-immr+1)

	case 2: // UBFM: the logical shifts and the zero extends alias it
		switch {
		case imms != width-1 && imms+1 == immr:
			return fmt.Sprintf("lsl %s, %s, #%d", d, s, width-1-imms)
		case imms == width-1:
			return fmt.Sprintf("lsr %s, %s, #%d", d, s, immr)
		case immr == 0 && imms == 7:
			return fmt.Sprintf("uxtb %s, %s", d, gpr(false, rn))
		case immr == 0 && imms == 15:
			return fmt.Sprintf("uxth %s, %s", d, gpr(false, rn))
		}
		return fmt.Sprintf("ubfx %s, %s, #%d, #%d", d, s, immr, imms-immr+1)
	}
	return ""
}

// decodeBitmask recovers the value a logical-immediate's (N, immr, imms) fields
// stand for -- the inverse of EncodeBitmask. The encoding names a run of ones of
// some length, rotated within an element, replicated across the register.
func decodeBitmask(n, immr, imms uint32, width int) (uint64, bool) {
	// The element size is found from the position of the highest set bit of
	// N:NOT(imms), which is why an all-ones imms is not encodable.
	x := n<<6 | ^imms&0x3f
	l := -1
	for i := 6; i >= 0; i-- {
		if x&(1<<uint(i)) != 0 {
			l = i
			break
		}
	}
	if l < 1 {
		return 0, false
	}
	esize := 1 << uint(l)
	if esize > width {
		return 0, false
	}
	levels := uint32(esize - 1)
	s, rot := imms&levels, immr&levels
	if s == levels {
		return 0, false // a full element of ones would replicate to all-ones
	}
	elem := uint64(1)<<uint(s+1) - 1
	elem = elem>>rot | elem<<uint(esize-int(rot))
	if esize < 64 {
		elem &= 1<<uint(esize) - 1
	}
	v := elem
	for e := esize; e < width; e <<= 1 {
		v |= v << uint(e)
	}
	if width < 64 {
		v &= 1<<uint(width) - 1
	}
	return v, true
}

// --- data processing: register ---------------------------------------------

func disasmDPReg(w uint32) string {
	switch {
	case bits(w, 28, 24) == 0x0b:
		if bit(w, 21) {
			return disasmAddSubExt(w)
		}
		return disasmAddSubReg(w)
	case bits(w, 28, 24) == 0x0a:
		return disasmLogicalReg(w)
	case bits(w, 28, 21) == 0xd6 && !bit(w, 30):
		return disasmDP2(w)
	case bits(w, 28, 21) == 0xd6 && bit(w, 30):
		return disasmDP1(w)
	case bits(w, 28, 21) == 0xd4:
		return disasmCondSel(w)
	case bits(w, 28, 24) == 0x1b:
		return disasmDP3(w)
	}
	return ""
}

// shiftSuffix renders the shifted-register operand's ", lsl #n" tail, empty when
// the operand is unshifted.
func shiftSuffix(w uint32) string {
	amount := bits(w, 15, 10)
	if amount == 0 {
		return ""
	}
	return fmt.Sprintf(", %s #%d", shiftNames[bits(w, 23, 22)], amount)
}

func disasmAddSubReg(w uint32) string {
	w64, rd, rn, rm := bit(w, 31), w&0x1f, bits(w, 9, 5), bits(w, 20, 16)
	op := "add"
	if bit(w, 30) {
		op = "sub"
	}
	sh := shiftSuffix(w)
	if bit(w, 29) { // S
		op += "s"
		if rd == 31 { // the result is thrown away: this is a compare
			return fmt.Sprintf("cmp %s, %s%s", gpr(w64, rn), gpr(w64, rm), sh)
		}
	} else if op == "sub" && rn == 31 { // subtracting from zero is a negate
		return fmt.Sprintf("neg %s, %s%s", gpr(w64, rd), gpr(w64, rm), sh)
	}
	return fmt.Sprintf("%s %s, %s, %s%s", op, gpr(w64, rd), gpr(w64, rn), gpr(w64, rm), sh)
}

func disasmAddSubExt(w uint32) string {
	w64, rd, rn, rm := bit(w, 31), w&0x1f, bits(w, 9, 5), bits(w, 20, 16)
	option, imm3 := bits(w, 15, 13), bits(w, 12, 10)
	op := "add"
	if bit(w, 30) {
		op = "sub"
	}
	// The index register is 32-bit unless the option says to take it whole.
	idx := gpr(option&3 == 3, rm)
	ext := extendNames[option]
	if imm3 != 0 {
		ext += fmt.Sprintf(" #%d", imm3)
	}
	if bit(w, 29) { // S
		op += "s"
		if rd == 31 {
			return fmt.Sprintf("cmp %s, %s, %s", gprSP(w64, rn), idx, ext)
		}
	}
	return fmt.Sprintf("%s %s, %s, %s, %s", op, gprSP(w64, rd), gprSP(w64, rn), idx, ext)
}

func disasmLogicalReg(w uint32) string {
	w64, rd, rn, rm := bit(w, 31), w&0x1f, bits(w, 9, 5), bits(w, 20, 16)
	ops := [4]string{"and", "orr", "eor", "ands"}
	negated := [4]string{"bic", "orn", "eon", "bics"}
	op := ops[bits(w, 30, 29)]
	if bit(w, 21) { // N: the second operand is inverted
		op = negated[bits(w, 30, 29)]
	}
	sh := shiftSuffix(w)
	switch {
	case op == "orr" && rn == 31 && sh == "": // OR with zero is a move
		return fmt.Sprintf("mov %s, %s", gpr(w64, rd), gpr(w64, rm))
	case op == "orn" && rn == 31: // OR-NOT with zero is a bitwise NOT
		return fmt.Sprintf("mvn %s, %s%s", gpr(w64, rd), gpr(w64, rm), sh)
	case op == "ands" && rd == 31:
		return fmt.Sprintf("tst %s, %s%s", gpr(w64, rn), gpr(w64, rm), sh)
	}
	return fmt.Sprintf("%s %s, %s, %s%s", op, gpr(w64, rd), gpr(w64, rn), gpr(w64, rm), sh)
}

func disasmDP2(w uint32) string {
	op, ok := map[uint32]string{2: "udiv", 3: "sdiv", 8: "lsl", 9: "lsr", 10: "asr", 11: "ror"}[bits(w, 15, 10)]
	if !ok {
		return ""
	}
	w64 := bit(w, 31)
	return fmt.Sprintf("%s %s, %s, %s", op, gpr(w64, w&0x1f), gpr(w64, bits(w, 9, 5)), gpr(w64, bits(w, 20, 16)))
}

func disasmDP1(w uint32) string {
	op, ok := map[uint32]string{0: "rbit", 1: "rev16", 4: "clz", 5: "cls"}[bits(w, 15, 10)]
	if !ok {
		return ""
	}
	w64 := bit(w, 31)
	return fmt.Sprintf("%s %s, %s", op, gpr(w64, w&0x1f), gpr(w64, bits(w, 9, 5)))
}

func disasmCondSel(w uint32) string {
	w64, rd, rn, rm := bit(w, 31), w&0x1f, bits(w, 9, 5), bits(w, 20, 16)
	cond := Cond(bits(w, 15, 12))
	ops := [2][2]string{{"csel", "csinc"}, {"csinv", "csneg"}}
	op := ops[bits(w, 30, 30)][bits(w, 11, 10)]
	// CSET reads as "1 when cond": CSINC of the two zero registers under the
	// inverted condition. It only reads that way when the condition is invertible.
	if op == "csinc" && rn == 31 && rm == 31 && cond&0xe != 0xe {
		return fmt.Sprintf("cset %s, %s", gpr(w64, rd), condNames[cond.invert()])
	}
	return fmt.Sprintf("%s %s, %s, %s, %s", op, gpr(w64, rd), gpr(w64, rn), gpr(w64, rm), condNames[cond])
}

func disasmDP3(w uint32) string {
	if bits(w, 23, 21) != 0 {
		return "" // the widening multiplies, which we never emit
	}
	w64, rd, rn, rm, ra := bit(w, 31), w&0x1f, bits(w, 9, 5), bits(w, 20, 16), bits(w, 14, 10)
	op, alias := "madd", "mul"
	if bit(w, 15) {
		op, alias = "msub", "mneg"
	}
	if ra == 31 { // nothing to accumulate into: a plain multiply
		return fmt.Sprintf("%s %s, %s, %s", alias, gpr(w64, rd), gpr(w64, rn), gpr(w64, rm))
	}
	return fmt.Sprintf("%s %s, %s, %s, %s", op, gpr(w64, rd), gpr(w64, rn), gpr(w64, rm), gpr(w64, ra))
}

// --- loads and stores ------------------------------------------------------

// intLdSt names an integer load/store by its size and opc fields, and reports the
// width of the transfer register.
func intLdSt(size, opc uint32) (op string, w64, ok bool) {
	switch size<<2 | opc {
	case 0b00_00:
		return "strb", false, true
	case 0b00_01:
		return "ldrb", false, true
	case 0b00_10:
		return "ldrsb", true, true
	case 0b00_11:
		return "ldrsb", false, true
	case 0b01_00:
		return "strh", false, true
	case 0b01_01:
		return "ldrh", false, true
	case 0b01_10:
		return "ldrsh", true, true
	case 0b01_11:
		return "ldrsh", false, true
	case 0b10_00:
		return "str", false, true
	case 0b10_01:
		return "ldr", false, true
	case 0b10_10:
		return "ldrsw", true, true
	case 0b11_00:
		return "str", true, true
	case 0b11_01:
		return "ldr", true, true
	}
	return "", false, false
}

func disasmLdSt(w uint32) string {
	if s := disasmExclusive(w); s != "" {
		return s
	}
	switch {
	case bits(w, 29, 24) == 0x39: // unsigned offset, integer
		size, opc := bits(w, 31, 30), bits(w, 23, 22)
		op, w64, ok := intLdSt(size, opc)
		if !ok {
			return ""
		}
		return fmt.Sprintf("%s %s, %s", op, gpr(w64, w&0x1f),
			memImm(bits(w, 9, 5), bits(w, 21, 10)<<size))

	case bits(w, 29, 24) == 0x3d: // unsigned offset, FP/SIMD
		size, opc := bits(w, 31, 30), bits(w, 23, 22)
		op, name, scale := "", "", uint32(0)
		switch {
		case size == 2 || size == 3: // S and D
			op, name, scale = ldOrStr(opc), fpr(size == 3, w&0x1f), size
		case size == 0 && opc >= 2: // Q
			op, name, scale = ldOrStr(opc&1), fmt.Sprintf("q%d", w&0x1f), 4
		default:
			return ""
		}
		if op == "" {
			return ""
		}
		return fmt.Sprintf("%s %s, %s", op, name, memImm(bits(w, 9, 5), bits(w, 21, 10)<<scale))

	case bits(w, 29, 24) == 0x38 && bit(w, 21) && bits(w, 11, 10) == 0b10: // register offset
		size, opc := bits(w, 31, 30), bits(w, 23, 22)
		op, w64, ok := intLdSt(size, opc)
		if !ok {
			return ""
		}
		option, s := bits(w, 15, 13), bits(w, 12, 12)
		// The index is 32-bit unless the option takes the register whole; LSL is
		// the do-nothing extend and is only named when it also scales.
		idx := gpr(option&1 == 1, bits(w, 20, 16))
		ext := ""
		if option != ExtLSL {
			ext = ", " + extendNames[option]
		}
		if s == 1 {
			if option == ExtLSL {
				ext = ", lsl"
			}
			ext += fmt.Sprintf(" #%d", size)
		}
		return fmt.Sprintf("%s %s, [%s, %s%s]", op, gpr(w64, w&0x1f), gprSP(true, bits(w, 9, 5)), idx, ext)

	case bits(w, 29, 25) == 0x14 && bits(w, 24, 23) != 0: // load/store pair
		return disasmPair(w)
	}
	return ""
}

func ldOrStr(opc uint32) string {
	if opc == 1 {
		return "ldr"
	}
	if opc == 0 {
		return "str"
	}
	return ""
}

// memImm renders a base-plus-unsigned-offset memory operand; a zero offset is
// left off, as the assembler writes it.
func memImm(rn, off uint32) string {
	if off == 0 {
		return fmt.Sprintf("[%s]", gprSP(true, rn))
	}
	return fmt.Sprintf("[%s, #%d]", gprSP(true, rn), off)
}

func disasmPair(w uint32) string {
	opc, load, mode := bits(w, 31, 30), bit(w, 22), PairMode(bits(w, 24, 23))
	if opc == 1 {
		return "" // the FP pair forms, which we never emit
	}
	w64 := opc == 2
	scale := int32(4)
	if w64 {
		scale = 8
	}
	op := "stp"
	if load {
		op = "ldp"
	}
	imm := sext(bits(w, 21, 15), 7) * scale
	base := gprSP(true, bits(w, 9, 5))
	var mem string
	switch mode {
	case PostIndex:
		mem = fmt.Sprintf("[%s], #%d", base, imm)
	case PreIndex:
		mem = fmt.Sprintf("[%s, #%d]!", base, imm)
	default:
		mem = fmt.Sprintf("[%s]", base)
		if imm != 0 {
			mem = fmt.Sprintf("[%s, #%d]", base, imm)
		}
	}
	return fmt.Sprintf("%s %s, %s, %s", op, gpr(w64, w&0x1f), gpr(w64, bits(w, 14, 10)), mem)
}

// --- floating point --------------------------------------------------------

func disasmFP(w uint32) string {
	if s := disasmMovVec(w); s != "" {
		return s
	}
	if bits(w, 30, 24) != 0x1e || !bit(w, 21) {
		return ""
	}
	dbl, rd, rn, rm := bit(w, 22), w&0x1f, bits(w, 9, 5), bits(w, 20, 16)

	switch {
	case bits(w, 11, 10) == 0b10: // FP data processing (2 source)
		op, ok := map[uint32]string{0: "fmul", 1: "fdiv", 2: "fadd", 3: "fsub"}[bits(w, 15, 12)]
		if !ok {
			return ""
		}
		return fmt.Sprintf("%s %s, %s, %s", op, fpr(dbl, rd), fpr(dbl, rn), fpr(dbl, rm))

	case bits(w, 14, 10) == 0b10000: // FP data processing (1 source)
		switch bits(w, 20, 15) {
		case 0b000000:
			return fmt.Sprintf("fmov %s, %s", fpr(dbl, rd), fpr(dbl, rn))
		case 0b000001:
			return fmt.Sprintf("fabs %s, %s", fpr(dbl, rd), fpr(dbl, rn))
		case 0b000010:
			return fmt.Sprintf("fneg %s, %s", fpr(dbl, rd), fpr(dbl, rn))
		case 0b000100, 0b000101: // FCVT to the other width
			return fmt.Sprintf("fcvt %s, %s", fpr(!dbl, rd), fpr(dbl, rn))
		}
		return ""

	case bits(w, 14, 10) == 0b01000: // FP compare
		if bits(w, 4, 0) != 0 || bits(w, 15, 14) != 0 {
			return ""
		}
		return fmt.Sprintf("fcmp %s, %s", fpr(dbl, rn), fpr(dbl, rm))

	case bits(w, 15, 10) == 0: // conversion between FP and integer
		return disasmFPIntConv(w)
	}
	return ""
}

func disasmFPIntConv(w uint32) string {
	intW64, dbl, rd, rn := bit(w, 31), bit(w, 22), w&0x1f, bits(w, 9, 5)
	switch rmode, opcode := bits(w, 20, 19), bits(w, 18, 16); {
	case rmode == 0b11 && opcode == 0b000: // FP -> signed int, round toward zero
		return fmt.Sprintf("fcvtzs %s, %s", gpr(intW64, rd), fpr(dbl, rn))
	case rmode == 0b11 && opcode == 0b001: // FP -> unsigned int
		return fmt.Sprintf("fcvtzu %s, %s", gpr(intW64, rd), fpr(dbl, rn))
	case rmode == 0 && opcode == 0b010: // signed int -> FP
		return fmt.Sprintf("scvtf %s, %s", fpr(dbl, rd), gpr(intW64, rn))
	case rmode == 0 && opcode == 0b011: // unsigned int -> FP
		return fmt.Sprintf("ucvtf %s, %s", fpr(dbl, rd), gpr(intW64, rn))
	case rmode == 0 && opcode == 0b110: // FP bits -> integer register
		return fmt.Sprintf("fmov %s, %s", gpr(intW64, rd), fpr(dbl, rn))
	case rmode == 0 && opcode == 0b111: // integer bits -> FP register
		return fmt.Sprintf("fmov %s, %s", fpr(dbl, rd), gpr(intW64, rn))
	}
	return ""
}

// disasmMovVec decodes the whole-register vector copy, an ORR of a register with
// itself.
func disasmMovVec(w uint32) string {
	if w&0xffe0fc00 != 0x4ea01c00 {
		return ""
	}
	rm, rn := bits(w, 20, 16), bits(w, 9, 5)
	if rm != rn {
		return fmt.Sprintf("orr v%d.16b, v%d.16b, v%d.16b", w&0x1f, rn, rm)
	}
	return fmt.Sprintf("mov v%d.16b, v%d.16b", w&0x1f, rn)
}

// --- system ----------------------------------------------------------------

func disasmSystem(w uint32) string {
	// MRS <Xt>, <sysreg> and MSR <sysreg>, <Xt>: the register-transfer forms. The
	// high bits fix everything but the L bit (bit 20, read vs write) and the five
	// fields naming the system register, which live in bits 19..5 and which
	// sysRegByEncoding maps back for the set we name.
	read := w&0xfff00000 == 0xd5300000
	if read || w&0xfff00000 == 0xd5100000 {
		if name, ok := sysRegByEncoding[w&0xfffe0]; ok {
			if read {
				return fmt.Sprintf("mrs x%d, %s", w&0x1f, name)
			}
			return fmt.Sprintf("msr %s, x%d", name, w&0x1f)
		}
	}
	// SVC #imm16: the exception-generating group, with the syscall selector.
	if w&0xffe0001f == 0xd4000001 {
		return fmt.Sprintf("svc #%d", bits(w, 20, 5))
	}
	// The barrier group: DMB/DSB/ISB share the reserved bits and differ in the
	// low opcode field (bits 7:5), with the domain in CRm (bits 11:8).
	if w&0xfffff0df == 0xd50330df && bits(w, 7, 5) == 6 {
		return "isb" // only the sy form is emitted, printed bare as the reference does
	}
	if w&0xfffff09f == 0xd503309f { // DMB (opc 5) / DSB (opc 4)
		op := map[uint32]string{5: "dmb", 4: "dsb"}[bits(w, 7, 5)]
		if op != "" {
			if dom := barrierName(bits(w, 11, 8)); dom != "" {
				return op + " " + dom
			}
		}
	}
	return ""
}

// barrierName is the inverse of barrierByName, for the domains this assembler
// encodes.
func barrierName(crm uint32) string {
	switch BarrierOption(crm) {
	case BarrierSY:
		return "sy"
	case BarrierISH:
		return "ish"
	case BarrierISHST:
		return "ishst"
	case BarrierLD:
		return "ld"
	case BarrierST:
		return "st"
	}
	return ""
}

// disasmExclusive decodes the LDXR/STXR/LDAXR/STLXR forms. They live in the
// load/store encoding space, so Disasm reaches this from disasmLdSt.
func disasmExclusive(w uint32) string {
	if bits(w, 29, 24) != 0x8 || bits(w, 21, 21) != 0 || bits(w, 14, 10) != 31 {
		return "" // not the exclusive-access group
	}
	// size selects the access width and its mnemonic suffix: byte, halfword, word,
	// doubleword. The data register is W for all but the doubleword form.
	size := bits(w, 31, 30)
	suffix := [4]string{"b", "h", "", ""}[size]
	w64 := size == 3
	load := bits(w, 22, 22) == 1
	acqrel := bits(w, 15, 15) == 1
	rt, rn, rs := w&0x1f, bits(w, 9, 5), bits(w, 20, 16)

	// Bit 23 set is the ordered, non-exclusive pair (LDAR/STLR): no status
	// register, and acquire/release is implied.
	if bits(w, 23, 23) == 1 {
		if rs != 31 {
			return ""
		}
		mn := "stlr" + suffix
		if load {
			mn = "ldar" + suffix
		}
		return fmt.Sprintf("%s %s, [%s]", mn, gpr(w64, rt), gprSP(true, rn))
	}

	switch {
	case load && rs == 31:
		mn := "ldxr" + suffix
		if acqrel {
			mn = "ldaxr" + suffix
		}
		return fmt.Sprintf("%s %s, [%s]", mn, gpr(w64, rt), gprSP(true, rn))
	case !load:
		mn := "stxr" + suffix
		if acqrel {
			mn = "stlxr" + suffix
		}
		return fmt.Sprintf("%s %s, %s, [%s]", mn, gpr(false, rs), gpr(w64, rt), gprSP(true, rn))
	}
	return ""
}

// DisasmBytes renders a run of instruction words, one per line, as Disasm does
// each. The input must be a whole number of 4-byte words.
func DisasmBytes(text []byte) string {
	var b strings.Builder
	for i := 0; i+4 <= len(text); i += 4 {
		w := uint32(text[i]) | uint32(text[i+1])<<8 | uint32(text[i+2])<<16 | uint32(text[i+3])<<24
		b.WriteString(Disasm(w))
		b.WriteByte('\n')
	}
	return b.String()
}
