package bpf

import (
	"fmt"
	"strings"
)

// Asm renders the program as a readable instruction listing, one numbered line
// per 8-byte slot (a 64-bit-immediate load spans two slots). It is for
// inspection and tests, not for loading.
func (p *Prog) Asm() string {
	var sb strings.Builder
	for i := 0; i < len(p.Insns); i++ {
		in := p.Insns[i]
		text := ""
		if in.Op == clsLD|modeIMM|sizeDW && i+1 < len(p.Insns) {
			imm := int64(uint32(in.Imm)) | int64(p.Insns[i+1].Imm)<<32
			text = fmt.Sprintf("r%d = %d ll", in.Dst, imm)
			fmt.Fprintf(&sb, "%3d: %s\n", i, text)
			i++ // consume the wide immediate's second slot
			continue
		}
		fmt.Fprintf(&sb, "%3d: %s\n", i, disasmOne(in))
	}
	return sb.String()
}

func disasmOne(in Insn) string {
	cls := in.Op & 0x07
	switch cls {
	case clsALU64, clsALU:
		w := ""
		if cls == clsALU {
			w = "(u32) "
		}
		op := in.Op & 0xf0
		if op == aluNEG {
			return fmt.Sprintf("r%d = %s-r%d", in.Dst, w, in.Dst)
		}
		mn := aluName[op]
		if in.Op&srcX != 0 {
			return fmt.Sprintf("r%d %s %sr%d", in.Dst, mn, w, in.Src)
		}
		return fmt.Sprintf("r%d %s %s%d", in.Dst, mn, w, in.Imm)
	case clsLDX:
		return fmt.Sprintf("r%d = *(%s *)(r%d %+d)", in.Dst, sizeName(in.Op&0x18), in.Src, in.Off)
	case clsSTX:
		return fmt.Sprintf("*(%s *)(r%d %+d) = r%d", sizeName(in.Op&0x18), in.Dst, in.Off, in.Src)
	case clsST:
		return fmt.Sprintf("*(%s *)(r%d %+d) = %d", sizeName(in.Op&0x18), in.Dst, in.Off, in.Imm)
	case clsJMP:
		op := in.Op & 0xf0
		switch op {
		case jmpJA:
			return fmt.Sprintf("goto %+d", in.Off)
		case jmpCALL:
			return fmt.Sprintf("call %d", in.Imm)
		case jmpEXIT:
			return "exit"
		}
		mn := jmpName[op]
		if in.Op&srcX != 0 {
			return fmt.Sprintf("if r%d %s r%d goto %+d", in.Dst, mn, in.Src, in.Off)
		}
		return fmt.Sprintf("if r%d %s %d goto %+d", in.Dst, mn, in.Imm, in.Off)
	}
	return fmt.Sprintf("<op %#x>", in.Op)
}
