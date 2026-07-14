// Package bpf is a cg12 backend that lowers IR to Linux eBPF bytecode. eBPF is a
// 64-bit RISC-like ISA run by an in-kernel verifier+JIT: eleven 64-bit registers
// (r0 holds return values and helper results, r1-r5 pass arguments, r6-r9 are
// callee-saved, r10 is a read-only frame pointer into a 512-byte stack), fixed
// 8-byte instructions (one 16-byte form for a 64-bit immediate), and calls to
// numbered helper functions. This package encodes those instructions and
// renders them as bytecode or a text listing.
package bpf

import "encoding/binary"

// Reg is an eBPF register number (0-10).
type Reg uint8

const (
	R0 Reg = iota // return value / helper result
	R1            // argument 1 / scratch
	R2
	R3
	R4
	R5
	R6 // callee-saved
	R7
	R8
	R9
	R10 // read-only frame pointer
)

// Instruction classes (low 3 bits of the opcode).
const (
	clsLD    = 0x00
	clsLDX   = 0x01
	clsST    = 0x02
	clsSTX   = 0x03
	clsALU   = 0x04
	clsJMP   = 0x05
	clsALU64 = 0x07
)

// ALU/JMP operation codes (high 4 bits) and source flag (bit 3).
const (
	// ALU
	aluADD  = 0x00
	aluSUB  = 0x10
	aluMUL  = 0x20
	aluDIV  = 0x30
	aluOR   = 0x40
	aluAND  = 0x50
	aluLSH  = 0x60
	aluRSH  = 0x70
	aluNEG  = 0x80
	aluMOD  = 0x90
	aluXOR  = 0xa0
	aluMOV  = 0xb0
	aluARSH = 0xc0

	// JMP
	jmpJA   = 0x00
	jmpJEQ  = 0x10
	jmpJGT  = 0x20
	jmpJGE  = 0x30
	jmpJSET = 0x40
	jmpJNE  = 0x50
	jmpJSGT = 0x60
	jmpJSGE = 0x70
	jmpCALL = 0x80
	jmpEXIT = 0x90
	jmpJLT  = 0xa0
	jmpJLE  = 0xb0
	jmpJSLT = 0xc0
	jmpJSLE = 0xd0

	srcK = 0x00 // operand is the immediate
	srcX = 0x08 // operand is the source register
)

// Memory sizes and modes.
const (
	sizeW  = 0x00 // 32-bit word
	sizeH  = 0x08 // 16-bit halfword
	sizeB  = 0x10 // 8-bit byte
	sizeDW = 0x18 // 64-bit doubleword

	modeIMM = 0x00 // BPF_IMM (the 64-bit-immediate load)
	modeMEM = 0x60 // BPF_MEM (regular load/store)
)

// Insn is one eBPF instruction. A 64-bit-immediate load occupies two Insns; the
// second carries the high 32 bits of the immediate in Imm and is otherwise zero.
type Insn struct {
	Op  uint8
	Dst Reg
	Src Reg
	Off int16
	Imm int32
}

// Encode appends the instruction's 8 little-endian bytes to out.
func (in Insn) Encode(out []byte) []byte {
	var b [8]byte
	b[0] = in.Op
	b[1] = byte(in.Dst&0xf) | byte(in.Src&0xf)<<4
	binary.LittleEndian.PutUint16(b[2:], uint16(in.Off))
	binary.LittleEndian.PutUint32(b[4:], uint32(in.Imm))
	return append(out, b[:]...)
}

// --- constructors ----------------------------------------------------------

// Mov64Imm: dst = imm (sign-extended to 64 bits).
func Mov64Imm(dst Reg, imm int32) Insn {
	return Insn{Op: clsALU64 | aluMOV | srcK, Dst: dst, Imm: imm}
}

// Mov64Reg: dst = src.
func Mov64Reg(dst, src Reg) Insn {
	return Insn{Op: clsALU64 | aluMOV | srcX, Dst: dst, Src: src}
}

// Alu64Reg: dst op= src (64-bit).
func Alu64Reg(op uint8, dst, src Reg) Insn {
	return Insn{Op: clsALU64 | op | srcX, Dst: dst, Src: src}
}

// Alu64Imm: dst op= imm (64-bit).
func Alu64Imm(op uint8, dst Reg, imm int32) Insn {
	return Insn{Op: clsALU64 | op | srcK, Dst: dst, Imm: imm}
}

// Alu32Reg: dst op= src (32-bit, result zero-extended).
func Alu32Reg(op uint8, dst, src Reg) Insn {
	return Insn{Op: clsALU | op | srcX, Dst: dst, Src: src}
}

// LdxMem: dst = *(size *)(src + off).
func LdxMem(size uint8, dst, src Reg, off int16) Insn {
	return Insn{Op: clsLDX | modeMEM | size, Dst: dst, Src: src, Off: off}
}

// StxMem: *(size *)(dst + off) = src.
func StxMem(size uint8, dst Reg, off int16, src Reg) Insn {
	return Insn{Op: clsSTX | modeMEM | size, Dst: dst, Src: src, Off: off}
}

// JmpA: unconditional relative jump (off instructions from the next one).
func JmpA(off int16) Insn { return Insn{Op: clsJMP | jmpJA, Off: off} }

// JmpCondReg: if dst <op> src goto +off (64-bit compare).
func JmpCondReg(op uint8, dst, src Reg, off int16) Insn {
	return Insn{Op: clsJMP | op | srcX, Dst: dst, Src: src, Off: off}
}

// JmpCondImm: if dst <op> imm goto +off.
func JmpCondImm(op uint8, dst Reg, imm int32, off int16) Insn {
	return Insn{Op: clsJMP | op | srcK, Dst: dst, Imm: imm, Off: off}
}

// Call invokes helper function number helper.
func Call(helper int32) Insn { return Insn{Op: clsJMP | jmpCALL, Imm: helper} }

// Exit returns from the program (or function); the value is in r0.
func Exit() Insn { return Insn{Op: clsJMP | jmpEXIT} }

// LdImm64 loads a full 64-bit immediate into dst; it assembles to two Insns.
func LdImm64(dst Reg, imm int64) [2]Insn {
	return [2]Insn{
		{Op: clsLD | modeIMM | sizeDW, Dst: dst, Imm: int32(imm)},
		{Imm: int32(imm >> 32)},
	}
}

// A 64-bit-immediate load's source register selects a special immediate: a map
// file descriptor, or the address of a value inside a map (used for .rodata).
const (
	pseudoMapFD    = 1
	pseudoMapValue = 2
)

// LdMapFD loads a map's file descriptor into dst (a two-slot instruction with a
// BPF_PSEUDO_MAP_FD source). fd is 0 until the loader patches in the real map fd.
func LdMapFD(dst Reg, fd int32) [2]Insn {
	return [2]Insn{
		{Op: clsLD | modeIMM | sizeDW, Dst: dst, Src: pseudoMapFD, Imm: fd},
		{Imm: 0},
	}
}

// LdMapValue loads the address of offset off inside a map's single value into
// dst (a BPF_PSEUDO_MAP_VALUE load), for reading .rodata. fd is patched at load;
// off is fixed at compile time.
func LdMapValue(dst Reg, fd, off int32) [2]Insn {
	return [2]Insn{
		{Op: clsLD | modeIMM | sizeDW, Dst: dst, Src: pseudoMapValue, Imm: fd},
		{Imm: off},
	}
}

// --- program ---------------------------------------------------------------

// Prog is an assembled eBPF program: a flat instruction stream.
type Prog struct {
	Insns []Insn
}

// Bytes returns the encoded bytecode, ready for the bpf() syscall or an ELF
// .text section.
func (p *Prog) Bytes() []byte {
	out := make([]byte, 0, len(p.Insns)*8)
	for _, in := range p.Insns {
		out = in.Encode(out)
	}
	return out
}

// Len returns the number of instruction slots (each 8 bytes).
func (p *Prog) Len() int { return len(p.Insns) }

func sizeName(size uint8) string {
	switch size {
	case sizeB:
		return "u8"
	case sizeH:
		return "u16"
	case sizeW:
		return "u32"
	default:
		return "u64"
	}
}

var aluName = map[uint8]string{
	aluADD: "+=", aluSUB: "-=", aluMUL: "*=", aluDIV: "/=", aluOR: "|=",
	aluAND: "&=", aluLSH: "<<=", aluRSH: ">>=", aluMOD: "%=", aluXOR: "^=",
	aluARSH: "s>>=", aluMOV: "=",
}

var jmpName = map[uint8]string{
	jmpJEQ: "==", jmpJNE: "!=", jmpJGT: ">", jmpJGE: ">=", jmpJLT: "<", jmpJLE: "<=",
	jmpJSGT: "s>", jmpJSGE: "s>=", jmpJSLT: "s<", jmpJSLE: "s<=", jmpJSET: "&",
}
