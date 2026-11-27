package jit

import (
	"encoding/binary"
	"fmt"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

const machine = obj.EM_AARCH64

// Compile compiles a cg12 module to an object this package can load.
func Compile(m *ir.Module) (*obj.Object, error) {
	onGoThread(m)
	return arm64.CompileToObject(m)
}

// flushICache makes instructions written to [addr, addr+n) visible to
// instruction fetch: arm64 doesn't keep its caches coherent for code.
// See call_linux_arm64.s.
//
//go:noescape
func flushICache(addr, n uintptr)

// relocateArch patches the relocation at the start of b, whose address
// is p, to refer to v, the symbol's address plus the addend.
func relocateArch(b []byte, p, v int64, typ uint32) error {
	word := binary.LittleEndian.Uint32(b)
	// imm12 puts the low 12 bits of v, scaled down by 1<<shift, in an add
	// or a load or store's unsigned offset.
	imm12 := func(shift uint) error {
		lo := v & 0xfff
		if lo&(1<<shift-1) != 0 {
			return fmt.Errorf("misaligned for a %d-byte access", 1<<shift)
		}
		put32(b, word&^(0xfff<<10)|uint32(lo>>shift)<<10)
		return nil
	}
	// adr puts a 21-bit signed immediate in an adr or adrp.
	adr := func(imm int64) error {
		if !fits(imm, 21) {
			return errRange
		}
		u := uint32(imm) & (1<<21 - 1)
		put32(b, word&^(3<<29|0x7ffff<<5)|(u&3)<<29|(u>>2)<<5)
		return nil
	}
	switch typ {
	case obj.R_AARCH64_ABS64:
		return put64(b, uint64(v))
	case obj.R_AARCH64_ABS32:
		if !fits(v, 33) || v < -(1<<31) {
			return errRange
		}
		put32(b, uint32(v))
	case obj.R_AARCH64_CALL26, obj.R_AARCH64_JUMP26:
		d := v - p
		if d&3 != 0 || !fits(d>>2, 26) {
			return errRange
		}
		put32(b, word&^(1<<26-1)|uint32(d>>2)&(1<<26-1))
	case obj.R_AARCH64_ADR_PREL_LO21:
		return adr(v - p)
	case obj.R_AARCH64_ADR_PREL_PG_HI21:
		return adr(v>>12 - p>>12)
	case obj.R_AARCH64_ADD_ABS_LO12_NC, obj.R_AARCH64_LDST8_ABS_LO12_NC:
		return imm12(0)
	case obj.R_AARCH64_LDST16_ABS_LO12_NC:
		return imm12(1)
	case obj.R_AARCH64_LDST32_ABS_LO12_NC:
		return imm12(2)
	case obj.R_AARCH64_LDST64_ABS_LO12_NC:
		return imm12(3)
	case obj.R_AARCH64_LDST128_ABS_LO12_NC:
		return imm12(4)
	default:
		return fmt.Errorf("type %d is unsupported", typ)
	}
	return nil
}
