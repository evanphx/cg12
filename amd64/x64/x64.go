// Package x64 encodes x86-64 (AMD64) instructions directly into machine-code
// bytes, so the backend need not shell out to an external assembler. Unlike a
// fixed-width ISA, x86-64 instructions are variable length (1..15 bytes): a set
// of legacy/mandatory prefixes, an optional REX byte, one or more opcode bytes,
// an optional ModRM byte, an optional SIB byte, an optional displacement, and an
// optional immediate. Each function here appends one instruction's bytes to (and
// returns) a caller-owned buffer.
//
// Registers are the raw 4-bit encodings: 0..7 are RAX,RCX,RDX,RBX,RSP,RBP,RSI,
// RDI and 8..15 are R8..R15; the high bit rides in the REX prefix. XMM registers
// use the same 0..15 numbering. A bool selects the 64-bit (REX.W) operand size.
//
// The encodings are validated byte-for-byte against llvm-mc in the tests; the
// reference is used only to check correctness, never at runtime.
package x64

import "encoding/binary"

// Reg is a raw register number (0..15 for the general-purpose or XMM registers).
type Reg byte

// General-purpose registers, in encoding order.
const (
	RAX Reg = 0
	RCX Reg = 1
	RDX Reg = 2
	RBX Reg = 3
	RSP Reg = 4
	RBP Reg = 5
	RSI Reg = 6
	RDI Reg = 7
	R8  Reg = 8
	R9  Reg = 9
	R10 Reg = 10
	R11 Reg = 11
	R12 Reg = 12
	R13 Reg = 13
	R14 Reg = 14
	R15 Reg = 15
)

// XMM registers share the 0..15 numbering with the GPRs; the two files never mix
// them in a single ModRM field, so no separate type is needed.
const (
	XMM0 Reg = 0
	XMM1 Reg = 1
	XMM2 Reg = 2
	XMM3 Reg = 3
	XMM4 Reg = 4
	XMM5 Reg = 5
	XMM6 Reg = 6
	XMM7 Reg = 7
)

// Mem is a memory operand: [Base + Index*Scale + Disp], or [RIP + Disp] when RIP
// is set (used for PC-relative references to symbols and data). Scale is 1,2,4,8.
type Mem struct {
	Base     Reg
	Disp     int32
	Index    Reg
	Scale    byte
	HasIndex bool
	RIP      bool
}

// At builds a simple [base + disp] operand, the common backend addressing mode.
func At(base Reg, disp int32) Mem { return Mem{Base: base, Disp: disp} }

// RIPRel builds a [rip + disp] operand for PC-relative symbol references.
func RIPRel(disp int32) Mem { return Mem{RIP: true, Disp: disp} }

func lo3(r Reg) byte { return byte(r) & 7 }
func hi1(r Reg) byte { return (byte(r) >> 3) & 1 }

func fitsInt8(v int32) bool { return v >= -128 && v <= 127 }

func appendU16(b []byte, v uint16) []byte { return binary.LittleEndian.AppendUint16(b, v) }
func appendI32(b []byte, v int32) []byte  { return binary.LittleEndian.AppendUint32(b, uint32(v)) }
func appendI64(b []byte, v int64) []byte  { return binary.LittleEndian.AppendUint64(b, uint64(v)) }

// rex returns the REX prefix byte for the given W bit and the R/X/B extension
// bits, or 0 when no REX is required. force emits REX even when it would be zero,
// which the low byte registers SPL/BPL/SIL/DIL need to be addressable.
func rex(w bool, r, x, b byte, force bool) byte {
	var v byte
	if w {
		v |= 0x08
	}
	v |= (r & 1) << 2
	v |= (x & 1) << 1
	v |= b & 1
	if v == 0 && !force {
		return 0
	}
	return 0x40 | v
}

// encMem encodes the ModRM (and any SIB + displacement) bytes for a memory
// operand addressed with the 3-bit reg field. It returns the bytes plus the
// REX.X and REX.B extension bits contributed by the index and base registers.
func encMem(reg byte, m Mem) (out []byte, x, b byte) {
	reg &= 7
	if m.RIP {
		// mod=00, rm=101 selects RIP-relative with a disp32.
		out = append(out, reg<<3|5)
		return appendI32(out, m.Disp), 0, 0
	}
	base := lo3(m.Base)
	b = hi1(m.Base)

	var mod byte
	switch {
	case m.Disp == 0 && base != 5: // base != RBP/R13: mod=00 has no disp
		mod = 0
	case fitsInt8(m.Disp):
		mod = 1
	default:
		mod = 2
	}

	switch {
	case m.HasIndex:
		idx := lo3(m.Index)
		x = hi1(m.Index)
		out = append(out, mod<<6|reg<<3|4) // rm=100 selects a SIB byte
		out = append(out, scaleBits(m.Scale)<<6|idx<<3|base)
	case base == 4: // RSP/R12 base always needs a SIB byte
		out = append(out, mod<<6|reg<<3|4)
		out = append(out, 0<<6|4<<3|4) // scale=1, index=none, base=RSP/R12
	default:
		out = append(out, mod<<6|reg<<3|base)
	}

	switch mod {
	case 1:
		out = append(out, byte(m.Disp))
	case 2:
		out = appendI32(out, m.Disp)
	}
	return out, x, b
}

func scaleBits(s byte) byte {
	switch s {
	case 2:
		return 1
	case 4:
		return 2
	case 8:
		return 3
	default:
		return 0
	}
}

// modrmReg builds a register-direct ModRM byte (mod=11) with reg/rm fields.
func modrmReg(reg, rm Reg) byte { return 0b11<<6 | lo3(reg)<<3 | lo3(rm) }

// --- the two fundamental encoding shapes -----------------------------------

// op_rr encodes a two-operand instruction with a register-direct r/m operand:
// prefixes, REX, opcode bytes, then ModRM. reg is the ModRM.reg field, rm is the
// ModRM.rm field. force8 makes the low byte registers addressable.
func op_rr(buf []byte, pfx []byte, w bool, opcode []byte, reg, rm Reg, force8 bool) []byte {
	buf = append(buf, pfx...)
	if r := rex(w, hi1(reg), 0, hi1(rm), force8); r != 0 {
		buf = append(buf, r)
	}
	buf = append(buf, opcode...)
	return append(buf, modrmReg(reg, rm))
}

// op_rm encodes a two-operand instruction with a memory r/m operand.
func op_rm(buf []byte, pfx []byte, w bool, opcode []byte, reg Reg, m Mem, force8 bool) []byte {
	mrm, x, b := encMem(lo3(reg), m)
	buf = append(buf, pfx...)
	if r := rex(w, hi1(reg), x, b, force8); r != 0 {
		buf = append(buf, r)
	}
	buf = append(buf, opcode...)
	return append(buf, mrm...)
}

// --- MOV -------------------------------------------------------------------

// MovReg encodes MOV rm, reg (rd = rs) using the 89/8B form; for register-direct
// operands either direction works, so the store form 89 /r is used.
func MovReg(w bool, dst, src Reg) []byte {
	return op_rr(nil, nil, w, []byte{0x89}, src, dst, false)
}

// Store encodes MOV [mem], reg (widths: 8/16/32/64 via wbits).
func Store(wbits int, src Reg, m Mem) []byte {
	switch wbits {
	case 8:
		return op_rm(nil, nil, false, []byte{0x88}, src, m, byteForce(src))
	case 16:
		return op_rm(nil, []byte{0x66}, false, []byte{0x89}, src, m, false)
	case 64:
		return op_rm(nil, nil, true, []byte{0x89}, src, m, false)
	default:
		return op_rm(nil, nil, false, []byte{0x89}, src, m, false)
	}
}

// Load encodes MOV reg, [mem] (widths: 32/64; use MovzxLoad/MovsxLoad for narrow
// loads that also extend).
func Load(w bool, dst Reg, m Mem) []byte {
	return op_rm(nil, nil, w, []byte{0x8b}, dst, m, false)
}

// MovImm32 encodes MOV rd, imm32 sign-extended to 64 bits (C7 /0 id) when w, or
// the plain 32-bit MOV (B8+rd id) otherwise.
func MovImm32(w bool, dst Reg, imm int32) []byte {
	if w {
		out := op_rr(nil, nil, true, []byte{0xc7}, 0, dst, false)
		return appendI32(out, imm)
	}
	var buf []byte
	if r := rex(false, 0, 0, hi1(dst), false); r != 0 {
		buf = append(buf, r)
	}
	buf = append(buf, 0xb8+lo3(dst))
	return appendI32(buf, imm)
}

// MovImm64 encodes MOVABS rd, imm64 (REX.W B8+rd io), the only way to load a full
// 64-bit immediate.
func MovImm64(dst Reg, imm int64) []byte {
	buf := []byte{rex(true, 0, 0, hi1(dst), false)}
	buf = append(buf, 0xb8+lo3(dst))
	return appendI64(buf, imm)
}

// StoreImm32 encodes MOV [mem], imm32 (C7 /0 id) at the given width.
func StoreImm32(wbits int, m Mem, imm int32) []byte {
	var out []byte
	switch wbits {
	case 16:
		out = op_rm(nil, []byte{0x66}, false, []byte{0xc7}, 0, m, false)
		return appendU16(out, uint16(imm))
	case 64:
		out = op_rm(nil, nil, true, []byte{0xc7}, 0, m, false)
	default:
		out = op_rm(nil, nil, false, []byte{0xc7}, 0, m, false)
	}
	return appendI32(out, imm)
}

func byteForce(r Reg) bool { return r >= RSP && r <= RDI } // SPL/BPL/SIL/DIL need REX

// --- integer ALU: register forms -------------------------------------------

// The classic ALU ops share the "op r/m, reg" encoding at 0x01+base*8. dst is the
// r/m operand, src the reg operand (dst op= src).
type aluOp struct {
	rmReg byte // opcode for "op r/m, reg"
	ext   byte // /digit for the immediate form (81 /ext, 83 /ext)
}

var (
	opAdd = aluOp{0x01, 0}
	opOr  = aluOp{0x09, 1}
	opAnd = aluOp{0x21, 4}
	opSub = aluOp{0x29, 5}
	opXor = aluOp{0x31, 6}
	opCmp = aluOp{0x39, 7}
)

func aluRR(o aluOp, w bool, dst, src Reg) []byte {
	return op_rr(nil, nil, w, []byte{o.rmReg}, src, dst, false)
}

// aluImm encodes "op r/m, imm": 83 /ext ib when the immediate fits a sign-extended
// byte, else 81 /ext id.
func aluImm(o aluOp, w bool, dst Reg, imm int32) []byte {
	if fitsInt8(imm) {
		out := op_rr(nil, nil, w, []byte{0x83}, Reg(o.ext), dst, false)
		return append(out, byte(imm))
	}
	out := op_rr(nil, nil, w, []byte{0x81}, Reg(o.ext), dst, false)
	return appendI32(out, imm)
}

// AddReg/SubReg/AndReg/OrReg/XorReg encode "dst op= src".
func AddReg(w bool, dst, src Reg) []byte { return aluRR(opAdd, w, dst, src) }
func SubReg(w bool, dst, src Reg) []byte { return aluRR(opSub, w, dst, src) }
func AndReg(w bool, dst, src Reg) []byte { return aluRR(opAnd, w, dst, src) }
func OrReg(w bool, dst, src Reg) []byte  { return aluRR(opOr, w, dst, src) }
func XorReg(w bool, dst, src Reg) []byte { return aluRR(opXor, w, dst, src) }

// CmpReg encodes CMP a, b (sets flags from a - b).
func CmpReg(w bool, a, b Reg) []byte { return aluRR(opCmp, w, a, b) }

// AddImm/SubImm/AndImm/OrImm/XorImm/CmpImm encode "dst op= imm".
func AddImm(w bool, dst Reg, imm int32) []byte { return aluImm(opAdd, w, dst, imm) }
func SubImm(w bool, dst Reg, imm int32) []byte { return aluImm(opSub, w, dst, imm) }
func AndImm(w bool, dst Reg, imm int32) []byte { return aluImm(opAnd, w, dst, imm) }
func OrImm(w bool, dst Reg, imm int32) []byte  { return aluImm(opOr, w, dst, imm) }
func XorImm(w bool, dst Reg, imm int32) []byte { return aluImm(opXor, w, dst, imm) }
func CmpImm(w bool, dst Reg, imm int32) []byte { return aluImm(opCmp, w, dst, imm) }

// TestReg encodes TEST a, b (sets flags from a & b) via 85 /r.
func TestReg(w bool, a, b Reg) []byte { return op_rr(nil, nil, w, []byte{0x85}, b, a, false) }

// --- multiply / divide -----------------------------------------------------

// Imul encodes IMUL dst, src (dst *= src) via the two-operand 0F AF /r form.
func Imul(w bool, dst, src Reg) []byte {
	return op_rr(nil, nil, w, []byte{0x0f, 0xaf}, dst, src, false)
}

// ImulImm encodes IMUL dst, src, imm32 (69 /r id).
func ImulImm(w bool, dst, src Reg, imm int32) []byte {
	out := op_rr(nil, nil, w, []byte{0x69}, dst, src, false)
	return appendI32(out, imm)
}

// Idiv/Div encode signed/unsigned division of RDX:RAX by r/m (F7 /7, F7 /6). The
// quotient lands in RAX, the remainder in RDX; the caller must set up RDX first
// (Cqo/Cdq for signed, zeroing RDX for unsigned).
func Idiv(w bool, src Reg) []byte { return op_rr(nil, nil, w, []byte{0xf7}, 7, src, false) }
func Div(w bool, src Reg) []byte  { return op_rr(nil, nil, w, []byte{0xf7}, 6, src, false) }

// Cqo/Cdq sign-extend RAX into RDX:RAX (64/32 bit) ahead of a signed divide.
func Cqo() []byte { return []byte{rex(true, 0, 0, 0, false), 0x99} }
func Cdq() []byte { return []byte{0x99} }

// Neg/Not encode NEG/NOT r/m (F7 /3, F7 /2).
func Neg(w bool, dst Reg) []byte { return op_rr(nil, nil, w, []byte{0xf7}, 3, dst, false) }
func Not(w bool, dst Reg) []byte { return op_rr(nil, nil, w, []byte{0xf7}, 2, dst, false) }

// --- shifts ----------------------------------------------------------------

// Shl/Shr/Sar by CL (D3 /4, /5, /7): the shift count is taken from CL.
func ShlCL(w bool, dst Reg) []byte { return op_rr(nil, nil, w, []byte{0xd3}, 4, dst, false) }
func ShrCL(w bool, dst Reg) []byte { return op_rr(nil, nil, w, []byte{0xd3}, 5, dst, false) }
func SarCL(w bool, dst Reg) []byte { return op_rr(nil, nil, w, []byte{0xd3}, 7, dst, false) }

// Shl/Shr/Sar by an immediate (C1 /ext ib).
func ShlImm(w bool, dst Reg, imm byte) []byte { return shiftImm(w, 4, dst, imm) }
func ShrImm(w bool, dst Reg, imm byte) []byte { return shiftImm(w, 5, dst, imm) }
func SarImm(w bool, dst Reg, imm byte) []byte { return shiftImm(w, 7, dst, imm) }

func shiftImm(w bool, ext byte, dst Reg, imm byte) []byte {
	out := op_rr(nil, nil, w, []byte{0xc1}, Reg(ext), dst, false)
	return append(out, imm)
}

// --- sign / zero extension -------------------------------------------------

// Movzx/Movsx byte and word to 32/64. The destination width is chosen by w; the
// source width by the opcode. These take a register source.
func MovzxByte(w bool, dst, src Reg) []byte {
	return op_rr(nil, nil, w, []byte{0x0f, 0xb6}, dst, src, byteForce(src))
}
func MovzxWord(w bool, dst, src Reg) []byte {
	return op_rr(nil, nil, w, []byte{0x0f, 0xb7}, dst, src, false)
}
func MovsxByte(w bool, dst, src Reg) []byte {
	return op_rr(nil, nil, w, []byte{0x0f, 0xbe}, dst, src, byteForce(src))
}
func MovsxWord(w bool, dst, src Reg) []byte {
	return op_rr(nil, nil, w, []byte{0x0f, 0xbf}, dst, src, false)
}

// Movsxd encodes MOVSXD r64, r/m32 (63 /r): sign-extend a 32-bit value to 64.
func Movsxd(dst, src Reg) []byte { return op_rr(nil, nil, true, []byte{0x63}, dst, src, false) }

// MovsxdLoad encodes MOVSXD r64, [mem]: load a 32-bit value and sign-extend it.
func MovsxdLoad(dst Reg, m Mem) []byte { return op_rm(nil, nil, true, []byte{0x63}, dst, m, false) }

// Narrow memory loads with extension: load a byte/word from memory into a 32/64
// bit register, zero- or sign-extended.
func MovzxLoadByte(w bool, dst Reg, m Mem) []byte {
	return op_rm(nil, nil, w, []byte{0x0f, 0xb6}, dst, m, false)
}
func MovzxLoadWord(w bool, dst Reg, m Mem) []byte {
	return op_rm(nil, nil, w, []byte{0x0f, 0xb7}, dst, m, false)
}
func MovsxLoadByte(w bool, dst Reg, m Mem) []byte {
	return op_rm(nil, nil, w, []byte{0x0f, 0xbe}, dst, m, false)
}
func MovsxLoadWord(w bool, dst Reg, m Mem) []byte {
	return op_rm(nil, nil, w, []byte{0x0f, 0xbf}, dst, m, false)
}

// --- lea -------------------------------------------------------------------

// Lea encodes LEA reg, [mem] (8D /r): compute the effective address, don't
// dereference. With a RIP-relative operand this materializes a symbol address.
func Lea(w bool, dst Reg, m Mem) []byte {
	return op_rm(nil, nil, w, []byte{0x8d}, dst, m, false)
}

// AddImm32 encodes ADD rd, imm32 always in the 32-bit-immediate form (81 /0 id),
// so the immediate is a 4-byte field a relocation can patch (used for TLS).
func AddImm32(w bool, dst Reg, imm int32) []byte {
	out := op_rr(nil, nil, w, []byte{0x81}, 0, dst, false)
	return appendI32(out, imm)
}

// MovFSZero encodes MOV rd, %fs:0 (a thread-pointer self-load): the value at
// offset 0 of the %fs segment is the thread pointer, the base for thread-local
// storage in the local-exec model.
func MovFSZero(dst Reg) []byte {
	rex := byte(0x48) | hi1(dst)<<2 // REX.W + R bit
	modrm := lo3(dst)<<3 | 4        // mod=00, reg=dst, rm=100 (SIB)
	return []byte{0x64, rex, 0x8b, modrm, 0x25, 0, 0, 0, 0}
}
