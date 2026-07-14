package bpf

import (
	"testing"

	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compileC compiles a C source with one function to optimized cg12 IR and
// returns that function.
func compileC(t *testing.T, src string) *ir.Func {
	t.Helper()
	m, err := cc.Compile("prog.c", src)
	require.NoError(t, err)
	opt.OptimizeModule(m)
	for _, f := range m.Funcs {
		if f.Start != nil {
			return f
		}
	}
	t.Fatal("no function compiled")
	return nil
}

// TestEncode checks a handful of instructions against their known eBPF encodings
// (opcode, packed registers, offset, immediate).
func TestEncode(t *testing.T) {
	cases := []struct {
		in   Insn
		want []byte
	}{
		{Mov64Imm(R0, 0), []byte{0xb7, 0, 0, 0, 0, 0, 0, 0}},
		{Mov64Imm(R1, 5), []byte{0xb7, 0x01, 0, 0, 5, 0, 0, 0}},
		{Exit(), []byte{0x95, 0, 0, 0, 0, 0, 0, 0}},
		{Alu64Reg(aluADD, R1, R2), []byte{0x0f, 0x21, 0, 0, 0, 0, 0, 0}}, // r1 += r2
		{LdxMem(sizeDW, R1, R10, -8), []byte{0x79, 0xa1, 0xf8, 0xff, 0, 0, 0, 0}},
		{StxMem(sizeDW, R10, -16, R2), []byte{0x7b, 0x2a, 0xf0, 0xff, 0, 0, 0, 0}},
		{Call(14), []byte{0x85, 0, 0, 0, 14, 0, 0, 0}},
		{JmpCondReg(jmpJEQ, R1, R2, 3), []byte{0x1d, 0x21, 3, 0, 0, 0, 0, 0}},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.in.Encode(nil), disasmOne(c.in))
	}

	// A 64-bit immediate load assembles to two eight-byte slots.
	w := LdImm64(R1, 0x1122334455667788)
	got := append(w[0].Encode(nil), w[1].Encode(nil)...)
	assert.Equal(t, []byte{0x18, 0x01, 0, 0, 0x88, 0x77, 0x66, 0x55, 0, 0, 0, 0, 0x44, 0x33, 0x22, 0x11}, got)
}

// TestCompileArith compiles a small arithmetic function and checks that values
// land in registers (not the stack).
func TestCompileArith(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("madd", ir.ClsL)
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsL, e.Mul(ir.ClsL, a, b), a)) // a*b + a
	opt.OptimizeModule(m)

	p, err := Compile(f)
	require.NoError(t, err)
	asm := p.Asm()
	assert.Contains(t, asm, "exit")
	assert.NotContains(t, asm, "r10", "a register-only function should not touch the stack")
	// The result must be produced in r0 before exit.
	assert.Regexp(t, `r0 = r\d`, asm)
}

// TestCompileLoopAndCall exercises a loop with a phi and a helper call whose
// result is live across it (so it must occupy a callee-saved register).
func TestCompileLoopAndCall(t *testing.T) {
	src := `unsigned long bpf_ktime_get_ns(void);
	        int handle(void *ctx) {
	            unsigned long t = bpf_ktime_get_ns();
	            int s = 0;
	            for (int i = 0; i < 8; i++) s += i;
	            return (int)(t % 7) + s;
	        }`
	f := compileC(t, src)
	p, err := Compile(f)
	require.NoError(t, err)
	asm := p.Asm()
	assert.Contains(t, asm, "call 5", "bpf_ktime_get_ns is helper 5")
	// The helper result is live across the loop, so it lives in a callee-saved
	// register (r6-r9).
	assert.Regexp(t, `r[6-9] = r0`, asm)
	assert.Contains(t, asm, "exit")
}

// TestUnknownHelperRejected reports a clear error for an unknown helper.
func TestUnknownHelperRejected(t *testing.T) {
	src := `long definitely_not_a_helper(void); int f(void*c){ return definitely_not_a_helper(); }`
	f := compileC(t, src)
	_, err := Compile(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown helper")
}
