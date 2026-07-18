package a64

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Decode is the inverse of the encoders: encode a known form, decode the word,
// and the fields must come back. This pins the structured decoder the lifter
// reads against the same facts the text disassembler is validated on.
func TestDecodeFields(t *testing.T) {
	cases := []struct {
		name string
		word uint32
		want Inst
	}{
		{"add-reg", AddReg(true, 0, 1, 2), Inst{Op: OpAdd, W64: true, Rd: 0, Rn: 1, Rm: 2}},
		{"add-imm", AddImm(true, 0, 1, 5), Inst{Op: OpAdd, W64: true, Rd: 0, Rn: 1, Imm: 5, HasImm: true}},
		{"sub-reg-w", SubReg(false, 3, 4, 5), Inst{Op: OpSub, Rd: 3, Rn: 4, Rm: 5}},
		{"cmp-reg", CmpReg(true, 1, 2), Inst{Op: OpCmp, W64: true, Rn: 1, Rm: 2}},
		{"cmp-imm", CmpImm(true, 7, 42), Inst{Op: OpCmp, W64: true, Rn: 7, Imm: 42, HasImm: true}},
		{"neg", NegReg(true, 0, 3), Inst{Op: OpNeg, W64: true, Rd: 0, Rm: 3}},
		{"mov-reg", MovReg(true, 0, 1), Inst{Op: OpMov, W64: true, Rd: 0, Rm: 1}},
		{"movz", Movz(true, 5, 0x1234, 0), Inst{Op: OpMovz, W64: true, Rd: 5, Imm: 0x1234}},
		{"movk-lsl16", Movk(true, 5, 0xabcd, 16), Inst{Op: OpMovk, W64: true, Rd: 5, Imm: 0xabcd, ShiftAmt: 16}},
		{"mul", Mul(true, 0, 1, 2), Inst{Op: OpMul, W64: true, Rd: 0, Rn: 1, Rm: 2}},
		{"madd", Madd(true, 0, 1, 2, 3), Inst{Op: OpMadd, W64: true, Rd: 0, Rn: 1, Rm: 2, Ra: 3}},
		{"udiv", Udiv(true, 0, 1, 2), Inst{Op: OpUDiv, W64: true, Rd: 0, Rn: 1, Rm: 2}},
		{"sdiv", Sdiv(false, 0, 1, 2), Inst{Op: OpSDiv, Rd: 0, Rn: 1, Rm: 2}},
		{"and", AndReg(true, 0, 1, 2), Inst{Op: OpAnd, W64: true, Rd: 0, Rn: 1, Rm: 2}},
		{"orr", OrrReg(true, 4, 5, 6), Inst{Op: OpOrr, W64: true, Rd: 4, Rn: 5, Rm: 6}},
		{"eor", EorReg(false, 4, 5, 6), Inst{Op: OpEor, Rd: 4, Rn: 5, Rm: 6}},
		{"lslv", Lslv(true, 0, 1, 2), Inst{Op: OpLslv, W64: true, Rd: 0, Rn: 1, Rm: 2}},
		{"clz", Clz(true, 0, 1), Inst{Op: OpClz, W64: true, Rd: 0, Rn: 1}},
		{"sxtw", Sxtw(0, 1), Inst{Op: OpSxtw, W64: true, Rd: 0, Rn: 1}},
		{"uxtb", Uxtb(0, 1), Inst{Op: OpUxtb, Rd: 0, Rn: 1}},
		{"cset-eq", Cset(true, 0, EQ), Inst{Op: OpCset, W64: true, Rd: 0, Cond: EQ}},
		{"cset-lt", Cset(true, 9, LT), Inst{Op: OpCset, W64: true, Rd: 9, Cond: LT}},
		{"b", B(16), Inst{Op: OpB, Target: 16}},
		{"bl", Bl(-8), Inst{Op: OpBl, Target: -8}},
		{"cbnz", Cbnz(true, 3, 8), Inst{Op: OpCbnz, W64: true, Rd: 3, Target: 8}},
		{"cbz", Cbz(false, 3, -4), Inst{Op: OpCbz, Rd: 3, Target: -4}},
		{"ret", Ret(30), Inst{Op: OpRet, Rn: 30}},
	}
	for _, c := range cases {
		got, ok := Decode(c.word)
		require.Truef(t, ok, "%s: word %#08x not decoded", c.name, c.word)
		require.Equalf(t, c.want, got, "%s", c.name)
	}
}
