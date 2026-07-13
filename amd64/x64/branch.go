package x64

// Cond is an x86-64 condition code (the low nibble of the Jcc/SETcc/CMOVcc
// opcodes). The names follow the flag test each performs.
type Cond byte

const (
	O  Cond = 0x0 // overflow
	NO Cond = 0x1
	B  Cond = 0x2 // below (unsigned <)
	AE Cond = 0x3 // above or equal (unsigned >=)
	E  Cond = 0x4 // equal
	NE Cond = 0x5
	BE Cond = 0x6 // below or equal (unsigned <=)
	A  Cond = 0x7 // above (unsigned >)
	S  Cond = 0x8 // sign
	NS Cond = 0x9
	P  Cond = 0xa // parity even
	NP Cond = 0xb
	L  Cond = 0xc // less (signed <)
	GE Cond = 0xd // greater or equal (signed >=)
	LE Cond = 0xe // less or equal (signed <=)
	G  Cond = 0xf // greater (signed >)
)

// --- unconditional control flow --------------------------------------------

// JmpRel encodes JMP rel32 (E9 cd): a PC-relative jump whose displacement is
// measured from the end of this 5-byte instruction.
func JmpRel(rel int32) []byte { return appendI32([]byte{0xe9}, rel) }

// CallRel encodes CALL rel32 (E8 cd), displacement from the instruction end.
func CallRel(rel int32) []byte { return appendI32([]byte{0xe8}, rel) }

// JmpReg encodes JMP r/m64 (FF /4): an indirect jump through a register.
func JmpReg(r Reg) []byte { return op_rr(nil, nil, false, []byte{0xff}, 4, r, false) }

// CallReg encodes CALL r/m64 (FF /2): an indirect call through a register.
func CallReg(r Reg) []byte { return op_rr(nil, nil, false, []byte{0xff}, 2, r, false) }

// JmpMem / CallMem encode the indirect forms through a memory operand.
func JmpMem(m Mem) []byte  { return op_rm(nil, nil, false, []byte{0xff}, 4, m, false) }
func CallMem(m Mem) []byte { return op_rm(nil, nil, false, []byte{0xff}, 2, m, false) }

// Ret encodes RET (C3).
func Ret() []byte { return []byte{0xc3} }

// --- conditional branches --------------------------------------------------

// Jcc encodes Jcc rel32 (0F 80+cc cd), displacement from the instruction end.
func Jcc(c Cond, rel int32) []byte {
	return appendI32([]byte{0x0f, 0x80 | byte(c)}, rel)
}

// Setcc encodes SETcc r/m8 (0F 90+cc /0): set the byte register to 0 or 1. The
// result usually needs a MovzxByte to widen into a full register.
func Setcc(c Cond, r Reg) []byte {
	return op_rr(nil, nil, false, []byte{0x0f, 0x90 | byte(c)}, 0, r, byteForce(r))
}

// SetccMem encodes SETcc into a memory byte.
func SetccMem(c Cond, m Mem) []byte {
	return op_rm(nil, nil, false, []byte{0x0f, 0x90 | byte(c)}, 0, m, false)
}

// Cmovcc encodes CMOVcc reg, r/m (0F 40+cc /r): move src into dst if cc holds.
func Cmovcc(w bool, c Cond, dst, src Reg) []byte {
	return op_rr(nil, nil, w, []byte{0x0f, 0x40 | byte(c)}, dst, src, false)
}

// --- stack -----------------------------------------------------------------

// Push encodes PUSH r64 (50+rd), Pop encodes POP r64 (58+rd). The 64-bit operand
// size is implicit; REX is emitted only to reach r8..r15.
func Push(r Reg) []byte {
	if h := hi1(r); h != 0 {
		return []byte{0x41, 0x50 + lo3(r)}
	}
	return []byte{0x50 + lo3(r)}
}

func Pop(r Reg) []byte {
	if h := hi1(r); h != 0 {
		return []byte{0x41, 0x58 + lo3(r)}
	}
	return []byte{0x58 + lo3(r)}
}

// --- misc ------------------------------------------------------------------

// Ud2 encodes UD2 (0F 0B): an undefined instruction used as a trap.
func Ud2() []byte { return []byte{0x0f, 0x0b} }

// Syscall encodes SYSCALL (0F 05): used by the freestanding test runtime.
func Syscall() []byte { return []byte{0x0f, 0x05} }
