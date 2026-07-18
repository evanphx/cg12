package a64

// Structured decoding: Decode turns one A64 word into a typed Inst, the way
// Disasm turns it into text. Where Disasm exists so tests can read our machine
// code, Decode exists so a lifter can rebuild IR from it — the operands come back
// as fields (registers, immediates, conditions, addressing) rather than a string.
//
// It decodes only what the backend emits, and only the integer/branch core so
// far (loads/stores and floating point arrive with the phases that lift them). A
// word it does not recognize returns ok=false, so a lifter refuses the function
// rather than guessing.

// Mnemonic identifies a decoded instruction. Aliases are resolved to the
// operation they mean (a subs-into-zr is Cmp, an orr-from-zr is Mov), so the
// lifter switches on semantics, not encodings.
type Mnemonic uint8

const (
	OpUnknown Mnemonic = iota

	// Moves.
	OpMovz
	OpMovk
	OpMovn
	OpMov // register copy (orr from zr)

	// Integer arithmetic. The second operand is Rm (optionally shifted) unless
	// HasImm, in which case it is Imm.
	OpAdd
	OpSub
	OpNeg
	OpCmp // subs into zr: sets a pending compare
	OpMul
	OpMadd
	OpMsub
	OpUDiv
	OpSDiv

	// Bitwise.
	OpAnd
	OpOrr
	OpEor
	OpBic
	OpOrn
	OpEon
	OpMvn

	// Shifts (variable, by Rm) and single-source.
	OpLslv
	OpLsrv
	OpAsrv
	OpRorv
	OpClz

	// Shifts and field ops by immediate (Imm = amount / lsb, Width = extract width).
	OpLsl
	OpLsr
	OpAsr
	OpSbfx
	OpUbfx

	// Extends (Rn is always the W source).
	OpSxtb
	OpSxth
	OpSxtw
	OpUxtb
	OpUxth

	// Conditional set: Rd = 1 if Cond else 0.
	OpCset

	// Loads and stores. Rd is the transfer register, Rn the base, MemOff the
	// offset; ldp/stp add Rt2 and Mode (pre/post/signed-offset writeback).
	OpLdr
	OpStr
	OpLdrb
	OpStrb
	OpLdrh
	OpStrh
	OpLdrsb
	OpLdrsh
	OpLdrsw
	OpLdp
	OpStp

	// Floating point. Dbl selects double vs single; conversions also use W64 for
	// the integer width. Rd/Rn/Rm are v-registers unless the op names a gp side.
	OpFadd
	OpFsub
	OpFmul
	OpFdiv
	OpFneg
	OpFmovReg    // d <- d
	OpFmovVec    // v.16b <- v.16b (a whole-register copy)
	OpFmovToGP   // gp <- fp bits
	OpFmovFromGP // fp <- gp bits
	OpFcmp
	OpFcvtStoD
	OpFcvtDtoS
	OpFcvtzs // fp -> signed int, round toward zero
	OpFcvtzu
	OpScvtf // signed int -> fp
	OpUcvtf
	OpLdrFP
	OpStrFP

	// Branches. Target is a pc-relative byte offset.
	OpB
	OpBl
	OpBcond
	OpCbz
	OpCbnz
	OpRet
	OpBr
	OpBlr
)

// Inst is a decoded A64 instruction. Only the fields an op uses are set.
type Inst struct {
	Op  Mnemonic
	W64 bool // X (true) vs W (false) operand width

	Rd, Rn, Rm Reg
	Ra         Reg // third source, for madd/msub (Rd = Ra +/- Rn*Rm)

	HasImm   bool  // the second operand is Imm, not Rm
	Imm      int64 // immediate operand, shift amount, or bitfield lsb
	Width    int64 // second bitfield parameter (extract width)
	Shift    uint32
	ShiftAmt uint32 // shifted-register operand: kind (lsl/lsr/asr/ror) + amount

	Cond Cond // BCOND / CSET
	Dbl  bool // floating-point width: double vs single

	// Memory addressing (loads/stores).
	MemOff int64
	Mode   PairMode // ldp/stp writeback mode
	Rt2    Reg      // second transfer register (ldp/stp)

	Target int32 // pc-relative byte offset (branches)
}

// Decode returns the typed form of one instruction word, and whether it was
// recognized. It mirrors Disasm's dispatch but stops at the integer/branch core.
func Decode(w uint32) (Inst, bool) {
	for _, try := range []func(uint32) (Inst, bool){
		decodeBranch,
		decodeDPImm,
		decodeDPReg,
		decodeLdSt,
		decodeFP,
	} {
		if in, ok := try(w); ok {
			return in, true
		}
	}
	return Inst{}, false
}

func decodeBranch(w uint32) (Inst, bool) {
	switch {
	case w&0x7c000000 == 0x14000000: // B / BL
		op := OpB
		if bit(w, 31) {
			op = OpBl
		}
		return Inst{Op: op, Target: sext(bits(w, 25, 0), 26) * 4}, true

	case w&0xff000010 == 0x54000000: // B.cond
		return Inst{Op: OpBcond, Cond: Cond(w & 0xf), Target: sext(bits(w, 23, 5), 19) * 4}, true

	case w&0x7e000000 == 0x34000000: // CBZ / CBNZ
		op := OpCbz
		if bit(w, 24) {
			op = OpCbnz
		}
		return Inst{Op: op, W64: bit(w, 31), Rd: Reg(w & 0x1f), Target: sext(bits(w, 23, 5), 19) * 4}, true

	case w&^uint32(0x3e0) == 0xd65f0000: // RET
		return Inst{Op: OpRet, Rn: Reg(bits(w, 9, 5))}, true
	case w&^uint32(0x3e0) == 0xd61f0000: // BR
		return Inst{Op: OpBr, Rn: Reg(bits(w, 9, 5))}, true
	case w&^uint32(0x3e0) == 0xd63f0000: // BLR
		return Inst{Op: OpBlr, Rn: Reg(bits(w, 9, 5))}, true
	}
	return Inst{}, false
}

func decodeDPImm(w uint32) (Inst, bool) {
	if bits(w, 28, 26) != 0b100 {
		return Inst{}, false
	}
	switch bits(w, 28, 23) {
	case 0x22, 0x23: // ADD/SUB (immediate)
		return decodeAddSubImm(w)
	case 0x24: // logical (immediate)
		return decodeLogicalImm(w)
	case 0x25: // MOVZ / MOVK / MOVN
		return decodeMoveWide(w)
	case 0x26: // bitfield
		return decodeBitfield(w)
	}
	return Inst{}, false
}

func decodeAddSubImm(w uint32) (Inst, bool) {
	w64, rd, rn, imm := bit(w, 31), Reg(w&0x1f), Reg(bits(w, 9, 5)), int64(bits(w, 21, 10))
	if bit(w, 22) {
		imm <<= 12
	}
	op := OpAdd
	if bit(w, 30) {
		op = OpSub
	}
	if bit(w, 29) && rd == 31 { // adds/subs into zr: a compare
		return Inst{Op: OpCmp, W64: w64, Rn: rn, Imm: imm, HasImm: true}, true
	}
	return Inst{Op: op, W64: w64, Rd: rd, Rn: rn, Imm: imm, HasImm: true}, true
}

func decodeLogicalImm(w uint32) (Inst, bool) {
	w64, rd, rn := bit(w, 31), Reg(w&0x1f), Reg(bits(w, 9, 5))
	val, ok := decodeBitmask(bits(w, 22, 22), bits(w, 21, 16), bits(w, 15, 10), int(widthOf(w64)))
	if !ok {
		return Inst{}, false
	}
	if bits(w, 30, 29) == 1 && rn == 31 { // orr from zr: a move of the immediate
		return Inst{Op: OpMov, W64: w64, Rd: rd, Imm: int64(val), HasImm: true}, true
	}
	op, ok := map[uint32]Mnemonic{0: OpAnd, 1: OpOrr, 2: OpEor}[bits(w, 30, 29)]
	if !ok {
		return Inst{}, false // ands (flag-setting) is not lifted
	}
	return Inst{Op: op, W64: w64, Rd: rd, Rn: rn, Imm: int64(val), HasImm: true}, true
}

func decodeMoveWide(w uint32) (Inst, bool) {
	op, ok := map[uint32]Mnemonic{0: OpMovn, 2: OpMovz, 3: OpMovk}[bits(w, 30, 29)]
	if !ok {
		return Inst{}, false
	}
	return Inst{Op: op, W64: bit(w, 31), Rd: Reg(w & 0x1f),
		Imm: int64(bits(w, 20, 5)), ShiftAmt: bits(w, 22, 21) * 16}, true
}

func decodeBitfield(w uint32) (Inst, bool) {
	w64, rd, rn := bit(w, 31), Reg(w&0x1f), Reg(bits(w, 9, 5))
	immr, imms, width := bits(w, 21, 16), bits(w, 15, 10), widthOf(w64)
	base := Inst{W64: w64, Rd: rd, Rn: rn}

	switch bits(w, 30, 29) {
	case 0: // SBFM: asr, sign-extends, sbfx
		switch {
		case imms == width-1:
			base.Op, base.Imm = OpAsr, int64(immr)
		case immr == 0 && imms == 7:
			base.Op = OpSxtb
		case immr == 0 && imms == 15:
			base.Op = OpSxth
		case immr == 0 && imms == 31 && w64:
			base.Op = OpSxtw
		default:
			base.Op, base.Imm, base.Width = OpSbfx, int64(immr), int64(imms-immr+1)
		}
		return base, true

	case 2: // UBFM: lsl, lsr, zero-extends, ubfx
		switch {
		case imms != width-1 && imms+1 == immr:
			base.Op, base.Imm = OpLsl, int64(width-1-imms)
		case imms == width-1:
			base.Op, base.Imm = OpLsr, int64(immr)
		case immr == 0 && imms == 7:
			base.Op = OpUxtb
		case immr == 0 && imms == 15:
			base.Op = OpUxth
		default:
			base.Op, base.Imm, base.Width = OpUbfx, int64(immr), int64(imms-immr+1)
		}
		return base, true
	}
	return Inst{}, false
}

func decodeDPReg(w uint32) (Inst, bool) {
	switch {
	case bits(w, 28, 24) == 0x0b && !bit(w, 21):
		return decodeAddSubReg(w)
	case bits(w, 28, 24) == 0x0a:
		return decodeLogicalReg(w)
	case bits(w, 28, 21) == 0xd6 && !bit(w, 30):
		return decodeDP2(w)
	case bits(w, 28, 21) == 0xd6 && bit(w, 30):
		return decodeDP1(w)
	case bits(w, 28, 21) == 0xd4:
		return decodeCondSel(w)
	case bits(w, 28, 24) == 0x1b:
		return decodeDP3(w)
	}
	return Inst{}, false
}

func decodeAddSubReg(w uint32) (Inst, bool) {
	w64, rd, rn, rm := bit(w, 31), Reg(w&0x1f), Reg(bits(w, 9, 5)), Reg(bits(w, 20, 16))
	shift, amt := bits(w, 23, 22), bits(w, 15, 10)
	sub := bit(w, 30)
	if bit(w, 29) && rd == 31 { // subs into zr: a compare
		return Inst{Op: OpCmp, W64: w64, Rn: rn, Rm: rm, Shift: shift, ShiftAmt: amt}, true
	}
	if sub && rn == 31 { // sub from zr: a negate
		return Inst{Op: OpNeg, W64: w64, Rd: rd, Rm: rm, Shift: shift, ShiftAmt: amt}, true
	}
	op := OpAdd
	if sub {
		op = OpSub
	}
	return Inst{Op: op, W64: w64, Rd: rd, Rn: rn, Rm: rm, Shift: shift, ShiftAmt: amt}, true
}

func decodeLogicalReg(w uint32) (Inst, bool) {
	w64, rd, rn, rm := bit(w, 31), Reg(w&0x1f), Reg(bits(w, 9, 5)), Reg(bits(w, 20, 16))
	shift, amt := bits(w, 23, 22), bits(w, 15, 10)
	plain := [4]Mnemonic{OpAnd, OpOrr, OpEor, OpUnknown}   // ands not lifted
	negated := [4]Mnemonic{OpBic, OpOrn, OpEon, OpUnknown} // bics not lifted
	op := plain[bits(w, 30, 29)]
	if bit(w, 21) {
		op = negated[bits(w, 30, 29)]
	}
	if op == OpUnknown {
		return Inst{}, false
	}
	if op == OpOrr && rn == 31 && amt == 0 { // orr from zr: a register move
		return Inst{Op: OpMov, W64: w64, Rd: rd, Rm: rm}, true
	}
	if op == OpOrn && rn == 31 { // orn from zr: a bitwise not
		return Inst{Op: OpMvn, W64: w64, Rd: rd, Rm: rm, Shift: shift, ShiftAmt: amt}, true
	}
	return Inst{Op: op, W64: w64, Rd: rd, Rn: rn, Rm: rm, Shift: shift, ShiftAmt: amt}, true
}

func decodeDP2(w uint32) (Inst, bool) {
	op, ok := map[uint32]Mnemonic{2: OpUDiv, 3: OpSDiv, 8: OpLslv, 9: OpLsrv, 10: OpAsrv, 11: OpRorv}[bits(w, 15, 10)]
	if !ok {
		return Inst{}, false
	}
	return Inst{Op: op, W64: bit(w, 31), Rd: Reg(w & 0x1f), Rn: Reg(bits(w, 9, 5)), Rm: Reg(bits(w, 20, 16))}, true
}

func decodeDP1(w uint32) (Inst, bool) {
	if bits(w, 15, 10) != 4 { // clz; rbit/rev16/cls are not lifted
		return Inst{}, false
	}
	return Inst{Op: OpClz, W64: bit(w, 31), Rd: Reg(w & 0x1f), Rn: Reg(bits(w, 9, 5))}, true
}

func decodeCondSel(w uint32) (Inst, bool) {
	w64, rd, rn, rm := bit(w, 31), Reg(w&0x1f), Reg(bits(w, 9, 5)), Reg(bits(w, 20, 16))
	cond := Cond(bits(w, 15, 12))
	// CSET is CSINC of the two zero registers under the inverted condition.
	if bits(w, 30, 30) == 0 && bits(w, 11, 10) == 1 && rn == 31 && rm == 31 && cond&0xe != 0xe {
		return Inst{Op: OpCset, W64: w64, Rd: rd, Cond: cond.invert()}, true
	}
	return Inst{}, false // csel/csinv/csneg are not on the IR path, so not lifted
}

func decodeDP3(w uint32) (Inst, bool) {
	if bits(w, 23, 21) != 0 {
		return Inst{}, false // widening multiplies, never emitted
	}
	w64, rd, rn, rm, ra := bit(w, 31), Reg(w&0x1f), Reg(bits(w, 9, 5)), Reg(bits(w, 20, 16)), Reg(bits(w, 14, 10))
	op := OpMadd
	if bit(w, 15) {
		op = OpMsub
	}
	if ra == 31 { // no accumulator: a plain multiply
		return Inst{Op: OpMul, W64: w64, Rd: rd, Rn: rn, Rm: rm}, true
	}
	return Inst{Op: op, W64: w64, Rd: rd, Rn: rn, Rm: rm, Ra: ra}, true
}

func decodeLdSt(w uint32) (Inst, bool) {
	switch {
	case bits(w, 29, 24) == 0x39: // unsigned offset, integer
		size, opc := bits(w, 31, 30), bits(w, 23, 22)
		op, w64, ok := ldStMnem(size, opc)
		if !ok {
			return Inst{}, false
		}
		return Inst{Op: op, W64: w64, Rd: Reg(w & 0x1f), Rn: Reg(bits(w, 9, 5)),
			MemOff: int64(bits(w, 21, 10) << size)}, true

	case bits(w, 29, 25) == 0x14 && bits(w, 24, 23) != 0: // load/store pair
		opc := bits(w, 31, 30)
		if opc == 1 {
			return Inst{}, false // the FP pair forms
		}
		w64 := opc == 2
		scale := int32(4)
		if w64 {
			scale = 8
		}
		op := OpStp
		if bit(w, 22) {
			op = OpLdp
		}
		return Inst{Op: op, W64: w64, Rd: Reg(w & 0x1f), Rt2: Reg(bits(w, 14, 10)),
			Rn: Reg(bits(w, 9, 5)), MemOff: int64(sext(bits(w, 21, 15), 7) * scale),
			Mode: PairMode(bits(w, 24, 23))}, true
	}
	return Inst{}, false
}

func decodeFP(w uint32) (Inst, bool) {
	// Whole-register copy (mov v.16b, an orr of a register with itself).
	if w&0xffe0fc00 == 0x4ea01c00 && bits(w, 20, 16) == bits(w, 9, 5) {
		return Inst{Op: OpFmovVec, Rd: Reg(w & 0x1f), Rn: Reg(bits(w, 9, 5))}, true
	}
	// FP scalar load/store, unsigned offset (S and D).
	if bits(w, 29, 24) == 0x3d {
		size := bits(w, 31, 30)
		if size != 2 && size != 3 {
			return Inst{}, false
		}
		op := OpLdrFP
		if bits(w, 23, 22) == 0 {
			op = OpStrFP
		} else if bits(w, 23, 22) != 1 {
			return Inst{}, false
		}
		return Inst{Op: op, Dbl: size == 3, Rd: Reg(w & 0x1f), Rn: Reg(bits(w, 9, 5)),
			MemOff: int64(bits(w, 21, 10) << size)}, true
	}

	if bits(w, 30, 24) != 0x1e || !bit(w, 21) {
		return Inst{}, false
	}
	dbl, rd, rn, rm := bit(w, 22), Reg(w&0x1f), Reg(bits(w, 9, 5)), Reg(bits(w, 20, 16))

	switch {
	case bits(w, 11, 10) == 0b10: // FP data processing (2 source)
		op, ok := map[uint32]Mnemonic{0: OpFmul, 1: OpFdiv, 2: OpFadd, 3: OpFsub}[bits(w, 15, 12)]
		if !ok {
			return Inst{}, false
		}
		return Inst{Op: op, Dbl: dbl, Rd: rd, Rn: rn, Rm: rm}, true

	case bits(w, 14, 10) == 0b10000: // FP data processing (1 source)
		switch bits(w, 20, 15) {
		case 0b000000:
			return Inst{Op: OpFmovReg, Dbl: dbl, Rd: rd, Rn: rn}, true
		case 0b000010:
			return Inst{Op: OpFneg, Dbl: dbl, Rd: rd, Rn: rn}, true
		case 0b000100: // FCVT single -> double
			return Inst{Op: OpFcvtStoD, Rd: rd, Rn: rn}, true
		case 0b000101: // FCVT double -> single
			return Inst{Op: OpFcvtDtoS, Rd: rd, Rn: rn}, true
		}
		return Inst{}, false

	case bits(w, 14, 10) == 0b01000: // FP compare
		if bits(w, 4, 0) != 0 || bits(w, 15, 14) != 0 {
			return Inst{}, false
		}
		return Inst{Op: OpFcmp, Dbl: dbl, Rn: rn, Rm: rm}, true

	case bits(w, 15, 10) == 0: // conversion between FP and integer
		w64, rmode, opcode := bit(w, 31), bits(w, 20, 19), bits(w, 18, 16)
		switch {
		case rmode == 0b11 && opcode == 0b000:
			return Inst{Op: OpFcvtzs, W64: w64, Dbl: dbl, Rd: rd, Rn: rn}, true
		case rmode == 0b11 && opcode == 0b001:
			return Inst{Op: OpFcvtzu, W64: w64, Dbl: dbl, Rd: rd, Rn: rn}, true
		case rmode == 0 && opcode == 0b010:
			return Inst{Op: OpScvtf, W64: w64, Dbl: dbl, Rd: rd, Rn: rn}, true
		case rmode == 0 && opcode == 0b011:
			return Inst{Op: OpUcvtf, W64: w64, Dbl: dbl, Rd: rd, Rn: rn}, true
		case rmode == 0 && opcode == 0b110:
			return Inst{Op: OpFmovToGP, W64: w64, Dbl: dbl, Rd: rd, Rn: rn}, true
		case rmode == 0 && opcode == 0b111:
			return Inst{Op: OpFmovFromGP, W64: w64, Dbl: dbl, Rd: rd, Rn: rn}, true
		}
		return Inst{}, false
	}
	return Inst{}, false
}

// ldStMnem names an integer load/store by its size and opc fields and reports the
// transfer-register width. It mirrors intLdSt.
func ldStMnem(size, opc uint32) (Mnemonic, bool, bool) {
	switch size<<2 | opc {
	case 0b00_00:
		return OpStrb, false, true
	case 0b00_01:
		return OpLdrb, false, true
	case 0b00_10:
		return OpLdrsb, true, true
	case 0b00_11:
		return OpLdrsb, false, true
	case 0b01_00:
		return OpStrh, false, true
	case 0b01_01:
		return OpLdrh, false, true
	case 0b01_10:
		return OpLdrsh, true, true
	case 0b01_11:
		return OpLdrsh, false, true
	case 0b10_00:
		return OpStr, false, true
	case 0b10_01:
		return OpLdr, false, true
	case 0b10_10:
		return OpLdrsw, true, true
	case 0b11_00:
		return OpStr, true, true
	case 0b11_01:
		return OpLdr, true, true
	}
	return OpUnknown, false, false
}
