package amd64_test

import (
	"bytes"
	"fmt"
	"math"
	"testing"

	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// Folding a constant into an instruction's immediate field, and the branchless
// conditional select. Both are covered twice over, and the two layers answer
// different questions.
//
// The encoding tests answer "did the fold happen at all". A test that only ran
// the code would pass just as happily against the register form this replaces --
// materialize the constant, then do a register-register operation -- so every
// case here also asserts that the *unfolded* instruction is absent. That is the
// half that keeps the optimization from silently disappearing.
//
// The execution tests answer "does the folded form compute the same number". They
// run natively on this x86-64 host, and the constants they use are chosen around
// the boundary that decides the fold: x86-64 has no 64-bit ALU immediate, so a
// 64-bit operation can fold only a constant already inside int32 and anything
// past that must still go through a register (imm.go). Getting that edge wrong
// yields a program that assembles, runs, and computes with the wrong number, so
// the values below step across it in both directions and in both signs.

// --- encoding ---------------------------------------------------------------

// immFuncText compiles a single one-parameter function and returns its machine
// code. The parameter is real, so the operand that is *not* the constant genuinely
// arrives in a register.
func immFuncText(t *testing.T, name string, cls ir.Cls, body func(f *ir.Func, b *ir.Block, x ir.Ref) ir.Ref) []byte {
	t.Helper()
	m := ir.NewModule()
	f := m.NewFunc(name, cls).Export()
	x := f.Param("x", cls)
	e := f.Entry()
	e.Ret(body(f, e, x))
	return bitsFuncText(t, m, name)
}

// immRequireNot asserts that an instruction is absent from the emitted code --
// the "the fold really fired" half of every case below.
func immRequireNot(t *testing.T, code, instr []byte, what string) {
	t.Helper()
	require.Falsef(t, bytes.Contains(code, instr),
		"%s: did not expect % x\nwithin emitted code % x", what, instr, code)
}

// The System V argument register a one-parameter function's operand arrives in,
// the first allocatable register its result is computed in (reg.go's
// intAllocOrder starts at RSI), and the scratch register the *unfolded* path
// would have staged the constant in.
const (
	immArgReg     = x64.RDI
	immDstReg     = x64.RSI
	immScratchReg = x64.R11 // gpScratch1
)

// Each ALU op with an 81/83 encoding folds its constant, at both operand widths,
// and stops materializing it into a scratch register.
func TestALUImmEncoding(t *testing.T) {
	ops := []struct {
		name string
		emit func(b *ir.Block, cls ir.Cls, x, k ir.Ref) ir.Ref
		enc  func(w bool, dst x64.Reg, imm int32) []byte
		reg  func(w bool, dst, src x64.Reg) []byte
	}{
		{"add", func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.Add(c, x, k) }, x64.AddImm, x64.AddReg},
		{"sub", func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.Sub(c, x, k) }, x64.SubImm, x64.SubReg},
		{"and", func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.And(c, x, k) }, x64.AndImm, x64.AndReg},
		{"or", func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.Or(c, x, k) }, x64.OrImm, x64.OrReg},
		{"xor", func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.Xor(c, x, k) }, x64.XorImm, x64.XorReg},
	}
	// 0x1234 is deliberately past the sign-extended imm8 the 83 encoding uses, so
	// both immediate encodings are exercised across the table.
	for _, k := range []int64{5, 0x1234} {
		for _, cls := range []ir.Cls{ir.ClsW, ir.ClsL} {
			w := cls == ir.ClsL
			for _, op := range ops {
				op := op
				t.Run(fmt.Sprintf("%s/%s/%d", op.name, cls, k), func(t *testing.T) {
					code := immFuncText(t, "op", cls, func(f *ir.Func, b *ir.Block, x ir.Ref) ir.Ref {
						return op.emit(b, cls, x, f.ConstInt(cls, k))
					})
					bitsRequireSeq(t, code, [][]byte{op.enc(w, immDstReg, int32(k))}, op.name+" with a constant operand")
					immRequireNot(t, code, x64.MovImm32(w, immScratchReg, int32(k)), "the constant must not reach a register")
					immRequireNot(t, code, op.reg(w, immDstReg, immScratchReg), "the register form must be gone")
				})
			}
		}
	}
}

// Multiplication gets the three-operand IMUL (69 /r id), which reads its source
// and writes its destination in one instruction -- so unlike the ops above it
// needs no copy into the destination first.
func TestMulImmEncoding(t *testing.T) {
	for _, cls := range []ir.Cls{ir.ClsW, ir.ClsL} {
		w := cls == ir.ClsL
		code := immFuncText(t, "op", cls, func(f *ir.Func, b *ir.Block, x ir.Ref) ir.Ref {
			return b.Mul(cls, x, f.ConstInt(cls, 7))
		})
		bitsRequireSeq(t, code, [][]byte{x64.ImulImm(w, immDstReg, immDstReg, 7)}, "multiply by a constant")
		immRequireNot(t, code, x64.MovImm32(w, immScratchReg, 7), "the constant must not reach a register")
		immRequireNot(t, code, x64.Imul(w, immDstReg, immScratchReg), "the register form must be gone")
	}
}

// A constant on the *left* of a commutative op folds too, by swapping the
// operands. A constant on the left of a subtraction does not: x86 encodes the
// immediate where the second operand goes, so `5 - x` has no immediate form and
// must keep the register one.
func TestCommutedImmEncoding(t *testing.T) {
	add := immFuncText(t, "op", ir.ClsL, func(f *ir.Func, b *ir.Block, x ir.Ref) ir.Ref {
		return b.Add(ir.ClsL, f.Long(9), x)
	})
	bitsRequireSeq(t, add, [][]byte{x64.AddImm(true, immDstReg, 9)}, "constant-first add")

	sub := immFuncText(t, "op", ir.ClsL, func(f *ir.Func, b *ir.Block, x ir.Ref) ir.Ref {
		return b.Sub(ir.ClsL, f.Long(9), x)
	})
	immRequireNot(t, sub, x64.SubImm(true, immDstReg, 9), "9 - x has no immediate form")
	bitsRequireSeq(t, sub, [][]byte{x64.SubReg(true, immDstReg, immArgReg)}, "constant-first subtract stays a register subtract")
}

// The imm32 boundary, at the encoding level. This is the case where being wrong
// is silent, so it is asserted from both sides: the largest constant that folds,
// and the smallest one past it that must not.
func TestImm32BoundaryEncoding(t *testing.T) {
	fold := func(t *testing.T, cls ir.Cls, k int64) []byte {
		return immFuncText(t, "op", cls, func(f *ir.Func, b *ir.Block, x ir.Ref) ir.Ref {
			return b.Add(cls, x, f.ConstInt(cls, k))
		})
	}
	for _, k := range []int64{math.MaxInt32, math.MinInt32} {
		code := fold(t, ir.ClsL, k)
		bitsRequireSeq(t, code, [][]byte{x64.AddImm(true, immDstReg, int32(k))},
			fmt.Sprintf("%d is exactly representable as a sign-extended imm32", k))
	}
	// One past the edge in either direction: the immediate field cannot hold the
	// value, so it goes back through MOVABS and a register add.
	for _, k := range []int64{math.MaxInt32 + 1, math.MinInt32 - 1, 0xffffffff, 1 << 40} {
		code := fold(t, ir.ClsL, k)
		bitsRequireSeq(t, code, [][]byte{
			x64.MovImm64(immScratchReg, k),
			x64.AddReg(true, immDstReg, immScratchReg),
		}, fmt.Sprintf("%#x does not fit a sign-extended imm32 and must stay in a register", k))
	}
	// At 32 bits there is no boundary at all: the operation reads and writes 32
	// bits, so every constant is expressible. 0xffffffff is the interesting one --
	// it is int32(-1), which even reaches the short imm8 form.
	code := fold(t, ir.ClsW, 0xffffffff)
	bitsRequireSeq(t, code, [][]byte{x64.AddImm(false, immDstReg, -1)}, "a 32-bit add folds every constant")
	immRequireNot(t, code, x64.MovImm32(false, immScratchReg, -1), "and never materializes one")
}

// A comparison against a constant folds into CMP's immediate, in both the shape
// that materializes a boolean and the shape that feeds the block's branch. The
// second is the one that matters and the one a selector-only fold would miss:
// mc.block emits a branch's comparison flags-only, without going through the
// instruction-selection chain at all.
func TestCmpImmEncoding(t *testing.T) {
	// Materialized boolean: cmp $5, %edi ; setl %sil ; movzbl %sil, %esi.
	boolean := immFuncText(t, "op", ir.ClsW, func(f *ir.Func, b *ir.Block, x ir.Ref) ir.Ref {
		return b.Cmp(ir.CmpSlt, ir.ClsW, x, f.Word(5))
	})
	bitsRequireSeq(t, boolean, [][]byte{
		x64.CmpImm(false, immArgReg, 5),
		x64.Setcc(x64.L, immDstReg),
	}, "compare against a constant")
	immRequireNot(t, boolean, x64.MovImm32(false, immScratchReg, 5), "the constant must not reach a register")

	// Fused branch: the same compare, consumed by the terminator.
	m := ir.NewModule()
	f := m.NewFunc("op", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	yes, no := f.NewBlock("yes"), f.NewBlock("no")
	e.Jnz(e.Cmp(ir.CmpSlt, ir.ClsW, x, f.Word(5)), yes, no)
	yes.Ret(f.Word(1))
	no.Ret(f.Word(0))
	branch := bitsFuncText(t, m, "op")
	bitsRequireSeq(t, branch, [][]byte{x64.CmpImm(false, immArgReg, 5)}, "fused compare-branch against a constant")
	immRequireNot(t, branch, x64.Setcc(x64.L, immDstReg), "the fused form materializes no boolean")
	immRequireNot(t, branch, x64.MovImm32(false, immScratchReg, 5), "the constant must not reach a register")

	// A 64-bit comparison against a constant too wide for imm32 keeps the register
	// form, for the same reason the arithmetic does.
	wideK := int64(1) << 40
	wide := immFuncText(t, "op", ir.ClsW, func(f *ir.Func, b *ir.Block, x ir.Ref) ir.Ref {
		return b.Cmp(ir.CmpSlt, ir.ClsW, b.Extsw(ir.ClsL, x), f.Long(wideK))
	})
	// int32(1<<40) is 0, which is what a fold that truncated instead of refusing
	// would have emitted.
	immRequireNot(t, wide, x64.CmpImm(true, immDstReg, int32(wideK)), "1<<40 is not a sign-extended imm32")
	bitsRequireSeq(t, wide, [][]byte{x64.MovImm64(immScratchReg, wideK)}, "a wide comparand stays in a register")
}

// An integer select becomes TEST + CMOVNE and contains no branch at all. The
// absence of a conditional jump is the assertion that matters: before this, every
// select was rewritten into a control-flow diamond by lower.Selects.
func TestSelectUsesCmov(t *testing.T) {
	for _, cls := range []ir.Cls{ir.ClsW, ir.ClsL} {
		w := cls == ir.ClsL
		code := immFuncText(t, "op", cls, func(f *ir.Func, b *ir.Block, x ir.Ref) ir.Ref {
			return b.Select(cls, x, f.ConstInt(cls, 11), f.ConstInt(cls, 22))
		})
		bitsRequireSeq(t, code, [][]byte{
			x64.TestReg(w, immArgReg, immArgReg),
			x64.Cmovcc(w, x64.NE, immDstReg, immScratchReg),
		}, "integer select")
		require.NotContainsf(t, string(code), "\x0f\x8f", "%s select must not branch", cls)
		require.NotContainsf(t, string(code), "\xe9", "%s select must not jump", cls)
	}
}

// When the condition and the result land in the same register -- which the
// allocator is free to do, since the condition dies at the select -- the
// condition has to be rescued before the false arm is written into the
// destination, or TEST reads the arm instead of the condition. This is the shape
// where that happens: `(x & 1) ? x : y` computes its condition into the register
// the result is assigned, so the sequence must copy it to RCX first.
//
// The failure it guards against is not a compile error but a silently wrong
// answer, and only for some allocations, so the check is on the emitted registers
// rather than on the result: TEST must read RCX, not the destination.
func TestSelectRescuesAliasedCondition(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("op", ir.ClsL).Export()
	x := f.Param("x", ir.ClsL)
	y := f.Param("y", ir.ClsL)
	e := f.Entry()
	e.Ret(e.Select(ir.ClsL, e.And(ir.ClsL, x, f.Long(1)), x, y))
	code := bitsFuncText(t, m, "op")

	bitsRequireSeq(t, code, [][]byte{
		x64.AndImm(true, immDstReg, 1),       // the condition, computed into the result's register
		x64.MovReg(true, x64.RCX, immDstReg), // rescued before the destination is overwritten
	}, "an aliased condition is copied out")
	bitsRequireSeq(t, code, [][]byte{
		x64.TestReg(true, x64.RCX, x64.RCX),
		x64.Cmovcc(true, x64.NE, immDstReg, immScratchReg),
	}, "the select tests the rescued copy")
	immRequireNot(t, code, x64.TestReg(true, immDstReg, immDstReg),
		"testing the destination would test the false arm, not the condition")

	// And it computes the right answer for both truth values.
	f2, e2 := entry(m)
	acc := e2.Cmp(ir.CmpNe, ir.ClsW, e2.Call(ir.ClsL, f2.Sym("op", 0), f2.Long(5), f2.Long(9)), f2.Long(5))
	acc = e2.Or(ir.ClsW, acc,
		e2.Shl(ir.ClsW, e2.Cmp(ir.CmpNe, ir.ClsW, e2.Call(ir.ClsL, f2.Sym("op", 0), f2.Long(4), f2.Long(9)), f2.Long(9)), f2.Word(1)))
	e2.Ret(acc)
	require.Equal(t, 0, runObj(t, m), "bit 0 = odd x, bit 1 = even x")
}

// A float select is the same CMOV, with both arms carried as bit patterns through
// general registers -- x86-64 has no SSE conditional move -- and the winner moved
// back into an XMM.
func TestFloatSelectUsesCmov(t *testing.T) {
	for _, cls := range []ir.Cls{ir.ClsS, ir.ClsD} {
		long := cls == ir.ClsD
		m := ir.NewModule()
		f := m.NewFunc("op", cls).Export()
		c := f.Param("c", ir.ClsW)
		e := f.Entry()
		e.Ret(e.Select(cls, c, f.Single(1.5), f.Single(2.5)))
		code := bitsFuncText(t, m, "op")
		bitsRequireSeq(t, code, [][]byte{
			x64.TestReg(false, immArgReg, immArgReg),
			x64.Cmovcc(long, x64.NE, x64.R10, x64.R11),
			x64.MovqToXmm(long, x64.XMM0, x64.R10),
		}, "float select")
		require.NotContainsf(t, string(code), "\xe9", "%s select must not jump", cls)
	}
}

// The register-operand path must be exactly what it was: this whole family
// declines an instruction with no constant operand, so selectCore's binInt still
// emits it. Two parameters, no constants, and the encodings are the pre-existing
// register ones.
func TestRegisterOperandsAreUnchanged(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("op", ir.ClsL).Export()
	x := f.Param("x", ir.ClsL)
	y := f.Param("y", ir.ClsL)
	e := f.Entry()
	e.Ret(e.Mul(ir.ClsL, e.Add(ir.ClsL, x, y), e.Sub(ir.ClsL, x, y)))
	code := bitsFuncText(t, m, "op")
	bitsRequireSeq(t, code, [][]byte{x64.AddReg(true, x64.RSI, x64.RDI)}, "register add")
	bitsRequireSeq(t, code, [][]byte{x64.SubReg(true, x64.R8, x64.RDI)}, "register subtract")
	bitsRequireSeq(t, code, [][]byte{x64.Imul(true, x64.RSI, x64.R8)}, "register multiply")
}

// --- execution --------------------------------------------------------------

// immCase is one `x OP k` evaluation with the answer Go computed for it.
type immCase struct {
	x, k, want int64
	op         func(b *ir.Block, cls ir.Cls, x, k ir.Ref) ir.Ref
}

// runImmCases compiles one helper function per case -- each carrying its own
// constant, since a folded constant is fixed at compile time and cannot be passed
// in -- calls each with the case's value, and folds one bit per mismatching case
// into runtest's exit code, so the failure names exactly which case broke.
//
// The value arrives as a parameter and so genuinely comes from a register; only
// the constant is folded, which is the shape the optimization is about.
func runImmCases(t *testing.T, cls ir.Cls, cases []immCase) {
	t.Helper()
	require.LessOrEqual(t, len(cases), 8, "the exit code carries one bit per case")

	m := ir.NewModule()
	for i, c := range cases {
		fn := m.NewFunc(fmt.Sprintf("op%d", i), cls)
		p := fn.Param("x", cls)
		b := fn.Entry()
		b.Ret(c.op(b, cls, p, fn.ConstInt(cls, c.k)))
	}

	f, e := entry(m)
	acc := f.Word(0)
	for i, c := range cases {
		got := e.Call(cls, f.Sym(fmt.Sprintf("op%d", i), 0), f.ConstInt(cls, c.x))
		bad := e.Cmp(ir.CmpNe, ir.ClsW, got, f.ConstInt(cls, c.want))
		acc = e.Or(ir.ClsW, acc, e.Shl(ir.ClsW, bad, f.Word(int64(i))))
	}
	e.Ret(acc)
	require.Equal(t, 0, runObj(t, m), "mismatching cases, one bit per case in order")
}

// immValues are the operands every 64-bit case below is evaluated at: zero, both
// signs, a value with bits in both halves, and both extremes of the range.
var immValues = []int64{0, 1, -1, 0x0123456789abcdef, math.MaxInt64, math.MinInt64}

// immConsts64 steps across the fold boundary in both directions and both signs.
// The first four fold (the first two into the short imm8 form), the last three do
// not, so one list exercises both code paths against the same expectations.
var immConsts64 = []int64{1, -1, math.MaxInt32, math.MinInt32, math.MaxInt32 + 1, math.MinInt32 - 1, 1 << 40}

func TestObjAddSubImm64(t *testing.T) {
	for _, k := range immConsts64 {
		k := k
		t.Run(fmt.Sprintf("k=%#x", k), func(t *testing.T) {
			var add, sub []immCase
			for _, x := range immValues {
				add = append(add, immCase{x: x, k: k, want: x + k,
					op: func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.Add(c, x, k) }})
				sub = append(sub, immCase{x: x, k: k, want: x - k,
					op: func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.Sub(c, x, k) }})
			}
			runImmCases(t, ir.ClsL, add)
			runImmCases(t, ir.ClsL, sub)
		})
	}
}

func TestObjBitwiseImm64(t *testing.T) {
	// Masks rather than small numbers: a bitwise op with a tiny constant cannot
	// tell a folded imm8 apart from a folded imm32 apart from a register.
	for _, k := range []int64{0xff, math.MaxInt32, math.MinInt32, 0xffffffff, -1} {
		k := k
		t.Run(fmt.Sprintf("k=%#x", k), func(t *testing.T) {
			var and, or, xor []immCase
			for _, x := range immValues {
				and = append(and, immCase{x: x, k: k, want: x & k,
					op: func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.And(c, x, k) }})
				or = append(or, immCase{x: x, k: k, want: x | k,
					op: func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.Or(c, x, k) }})
				xor = append(xor, immCase{x: x, k: k, want: x ^ k,
					op: func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.Xor(c, x, k) }})
			}
			runImmCases(t, ir.ClsL, and)
			runImmCases(t, ir.ClsL, or)
			runImmCases(t, ir.ClsL, xor)
		})
	}
}

func TestObjMulImm64(t *testing.T) {
	for _, k := range []int64{0, 1, -1, 7, math.MaxInt32, math.MinInt32, math.MaxInt32 + 1} {
		k := k
		t.Run(fmt.Sprintf("k=%d", k), func(t *testing.T) {
			var cases []immCase
			for _, x := range immValues {
				cases = append(cases, immCase{x: x, k: k, want: x * k,
					op: func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.Mul(c, x, k) }})
			}
			runImmCases(t, ir.ClsL, cases)
		})
	}
}

// The 32-bit width has its own run because every constant folds there -- the
// operation reads and writes 32 bits, so int32 truncation loses nothing -- and
// because the answers are 32-bit ones, computed here with Go's own int32.
func TestObjALUImm32(t *testing.T) {
	values := []int32{0, 1, -1, 0x12345678, math.MaxInt32, math.MinInt32}
	for _, k := range []int32{1, -1, 0x7fffffff, -0x80000000, 0x5a5a5a5a} {
		k := k
		t.Run(fmt.Sprintf("k=%#x", uint32(k)), func(t *testing.T) {
			var add, and, mul []immCase
			for _, x := range values {
				x := x
				add = append(add, immCase{x: int64(x), k: int64(k), want: int64(x + k),
					op: func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.Add(c, x, k) }})
				and = append(and, immCase{x: int64(x), k: int64(k), want: int64(x & k),
					op: func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.And(c, x, k) }})
				mul = append(mul, immCase{x: int64(x), k: int64(k), want: int64(x * k),
					op: func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref { return b.Mul(c, x, k) }})
			}
			runImmCases(t, ir.ClsW, add)
			runImmCases(t, ir.ClsW, and)
			runImmCases(t, ir.ClsW, mul)
		})
	}
}

// Every comparison predicate against a constant, at both widths. The unsigned
// predicates are the interesting ones: a negative constant is a very large
// unsigned one, so a fold that reasoned about magnitude rather than bits would
// answer these backwards.
func TestObjCmpImm(t *testing.T) {
	preds := []struct {
		cmp  ir.Cmp
		eval func(a, b int64) bool
	}{
		{ir.CmpEq, func(a, b int64) bool { return a == b }},
		{ir.CmpNe, func(a, b int64) bool { return a != b }},
		{ir.CmpSlt, func(a, b int64) bool { return a < b }},
		{ir.CmpSle, func(a, b int64) bool { return a <= b }},
		{ir.CmpSgt, func(a, b int64) bool { return a > b }},
		{ir.CmpSge, func(a, b int64) bool { return a >= b }},
		{ir.CmpUlt, func(a, b int64) bool { return uint64(a) < uint64(b) }},
		{ir.CmpUle, func(a, b int64) bool { return uint64(a) <= uint64(b) }},
		{ir.CmpUgt, func(a, b int64) bool { return uint64(a) > uint64(b) }},
		{ir.CmpUge, func(a, b int64) bool { return uint64(a) >= uint64(b) }},
	}
	for _, k := range []int64{0, -1, math.MaxInt32, math.MaxInt32 + 1} {
		for _, p := range preds {
			k, p := k, p
			t.Run(fmt.Sprintf("%d/k=%#x", p.cmp, k), func(t *testing.T) {
				var cases []immCase
				for _, x := range immValues {
					want := int64(0)
					if p.eval(x, k) {
						want = 1
					}
					cases = append(cases, immCase{x: x, k: k, want: want,
						op: func(b *ir.Block, c ir.Cls, x, k ir.Ref) ir.Ref {
							return b.Extuw(ir.ClsL, b.Cmp(p.cmp, ir.ClsW, x, k))
						}})
				}
				runImmCases(t, ir.ClsL, cases)
			})
		}
	}
}

// The same comparisons in the position that skips instruction selection: feeding
// the block's branch. A fold that only fired in the selector would pass the test
// above and miss this one entirely.
func TestObjFusedCmpImmBranch(t *testing.T) {
	m := ir.NewModule()
	for i, k := range immConsts64 {
		fn := m.NewFunc(fmt.Sprintf("op%d", i), ir.ClsL)
		x := fn.Param("x", ir.ClsL)
		b := fn.Entry()
		yes, no := fn.NewBlock("yes"), fn.NewBlock("no")
		b.Jnz(b.Cmp(ir.CmpSlt, ir.ClsL, x, fn.Long(k)), yes, no)
		yes.Ret(fn.Long(1))
		no.Ret(fn.Long(0))
	}

	f, e := entry(m)
	acc := f.Word(0)
	// One case per constant, evaluated at a value on each side of it.
	for i, k := range immConsts64 {
		for _, x := range []int64{math.MinInt64, math.MaxInt64} {
			want := int64(0)
			if x < k {
				want = 1
			}
			got := e.Call(ir.ClsL, f.Sym(fmt.Sprintf("op%d", i), 0), f.Long(x))
			bad := e.Cmp(ir.CmpNe, ir.ClsW, got, f.Long(want))
			acc = e.Or(ir.ClsW, acc, bad)
		}
	}
	e.Ret(acc)
	require.Equal(t, 0, runObj(t, m), "a fused compare against a constant answered wrongly")
}

// --- select -----------------------------------------------------------------

// TestObjSelectInt runs integer selects for both truth values of the condition
// and at both widths, with arms that are constants and arms that are not. The
// non-constant arms matter because CMOV has no immediate form: a constant arm has
// to be materialized first, and the two paths through the selector differ.
func TestObjSelectInt(t *testing.T) {
	for _, cls := range []ir.Cls{ir.ClsW, ir.ClsL} {
		cls := cls
		t.Run(cls.String(), func(t *testing.T) {
			m := ir.NewModule()

			// constSel(c) = c ? 11 : 22
			cs := m.NewFunc("constSel", cls)
			cc0 := cs.Param("c", ir.ClsW)
			cb := cs.Entry()
			cb.Ret(cb.Select(cls, cc0, cs.ConstInt(cls, 11), cs.ConstInt(cls, 22)))

			// regSel(c, a, b) = c ? a : b
			rs := m.NewFunc("regSel", cls)
			rc := rs.Param("c", ir.ClsW)
			ra := rs.Param("a", cls)
			rb := rs.Param("b", cls)
			rbk := rs.Entry()
			rbk.Ret(rbk.Select(cls, rc, ra, rb))

			f, e := entry(m)
			acc := f.Word(0)
			bit := 0
			check := func(got, want ir.Ref) {
				acc = e.Or(ir.ClsW, acc, e.Shl(ir.ClsW, e.Cmp(ir.CmpNe, ir.ClsW, got, want), f.Word(int64(bit))))
				bit++
			}
			check(e.Call(cls, f.Sym("constSel", 0), f.Word(1)), f.ConstInt(cls, 11))
			check(e.Call(cls, f.Sym("constSel", 0), f.Word(0)), f.ConstInt(cls, 22))
			// A non-zero condition that is not 1: TEST looks at every bit, so a
			// condition of 2 must select the true arm just as 1 does.
			check(e.Call(cls, f.Sym("constSel", 0), f.Word(2)), f.ConstInt(cls, 11))
			check(e.Call(cls, f.Sym("regSel", 0), f.Word(1), f.ConstInt(cls, -7), f.ConstInt(cls, 9)), f.ConstInt(cls, -7))
			check(e.Call(cls, f.Sym("regSel", 0), f.Word(0), f.ConstInt(cls, -7), f.ConstInt(cls, 9)), f.ConstInt(cls, 9))
			e.Ret(acc)
			require.Equal(t, 0, runObj(t, m), "one bit per failing select, in order")
		})
	}
}

// A float select must be a bit-exact choice between its arms and not an
// arithmetic operation, so the arms include a NaN and a negative zero -- the two
// values a mask- or arithmetic-based implementation would quietly alter. The
// comparison is on the raw bits for the same reason: -0.0 == 0.0 as a float.
func TestObjSelectFloat(t *testing.T) {
	nan := int64(0x7ff8000000000abc) // a NaN with a payload, so the payload is checked too
	negZero := int64(math.Float64bits(math.Copysign(0, -1)))

	m := ir.NewModule()
	sel := m.NewFunc("sel", ir.ClsL)
	c := sel.Param("c", ir.ClsW)
	a := sel.Param("a", ir.ClsD)
	b := sel.Param("b", ir.ClsD)
	sb := sel.Entry()
	// Cast back to an integer so the answer is compared bit for bit.
	sb.Ret(sb.Cast(ir.ClsL, sb.Select(ir.ClsD, c, a, b)))

	f, e := entry(m)
	acc := f.Word(0)
	bit := 0
	check := func(cond int64, want int64) {
		got := e.Call(ir.ClsL, f.Sym("sel", 0), f.Word(cond),
			e.Cast(ir.ClsD, f.Long(nan)), e.Cast(ir.ClsD, f.Long(negZero)))
		acc = e.Or(ir.ClsW, acc, e.Shl(ir.ClsW, e.Cmp(ir.CmpNe, ir.ClsW, got, f.Long(want)), f.Word(int64(bit))))
		bit++
	}
	check(1, nan)
	check(0, negZero)
	e.Ret(acc)
	require.Equal(t, 0, runObj(t, m), "bit 0 = the true arm, bit 1 = the false arm")
}

// Selects with their conditions, arms and results in spill slots rather than
// registers: the operands are reloaded through the scratch registers the select
// sequence also stages its true arm and its condition in, which is the one place
// the two could collide.
func TestObjSelectUnderRegisterPressure(t *testing.T) {
	m := ir.NewModule()
	op := m.NewFunc("op", ir.ClsL)
	x := op.Param("x", ir.ClsL)
	ob := op.Entry()

	// Twelve simultaneously-live selects against nine allocatable GPRs, so both
	// the condition and the arms are reached through spill slots, and the results
	// are committed out of the scratch register.
	var vals []ir.Ref
	want := int64(0)
	for i := 0; i < 12; i++ {
		cond := ob.And(ir.ClsL, ob.Shr(ir.ClsL, x, op.Long(int64(i))), op.Long(1))
		vals = append(vals, ob.Select(ir.ClsL, cond, op.Long(int64(i)+100), op.Long(int64(i)-100)))
		if (0x0123456789abcdef>>uint(i))&1 != 0 {
			want += int64(i) + 100
		} else {
			want += int64(i) - 100
		}
	}
	sum := op.Long(0)
	for i := len(vals) - 1; i >= 0; i-- {
		sum = ob.Add(ir.ClsL, sum, vals[i])
	}
	ob.Ret(sum)

	f, e := entry(m)
	got := e.Call(ir.ClsL, f.Sym("op", 0), f.Long(0x0123456789abcdef))
	e.Ret(e.Cmp(ir.CmpNe, ir.ClsW, got, f.Long(want)))
	require.Equal(t, 0, runObj(t, m))
}

// The immediate forms under register pressure: the operand and the result both
// live in spill slots, which is a different path on both ends (the operand is
// reloaded into a scratch register, the result stored back out of one) and the
// one place the fold could collide with the scratch registers it shares with the
// move machinery.
func TestObjImmUnderRegisterPressure(t *testing.T) {
	const seed = int64(0x0123456789abcdef)

	m := ir.NewModule()
	op := m.NewFunc("op", ir.ClsL)
	x := op.Param("x", ir.ClsL)
	ob := op.Entry()

	var vals []ir.Ref
	want := int64(0)
	for i := 0; i < 12; i++ {
		k := int64(i)*0x1000_0000 - 3 // small, large, and past-imm32 constants in one sweep
		vals = append(vals, ob.Add(ir.ClsL, ob.Xor(ir.ClsL, x, op.Long(k)), op.Long(int64(i))))
		want += (seed ^ k) + int64(i)
	}
	sum := op.Long(0)
	for i := len(vals) - 1; i >= 0; i-- {
		sum = ob.Add(ir.ClsL, sum, vals[i])
	}
	ob.Ret(sum)

	f, e := entry(m)
	got := e.Call(ir.ClsL, f.Sym("op", 0), f.Long(seed))
	e.Ret(e.Cmp(ir.CmpNe, ir.ClsW, got, f.Long(want)))
	require.Equal(t, 0, runObj(t, m))
}

// End to end through the C frontend, which is where the constants selection
// actually sees come from. Every expression here folds a literal into an
// instruction; the answers are written out so the test says what it expects
// rather than recomputing it the way the compiler would.
func TestObjImmFromC(t *testing.T) {
	src := `
int runtest(void) {
	int bad = 0;
	long x = 0x0123456789abcdefL;
	if ((x + 1)          != 0x0123456789abcdf0L) bad |= 1;
	if ((x - 0x7fffffff) != 0x0123456709abcdf0L) bad |= 2;
	if ((x & 0xffL)      != 0xefL)               bad |= 4;
	if ((x | 0xff00L)    != 0x0123456789abffefL) bad |= 8;
	if ((x ^ -1L)        != ~0x0123456789abcdefL) bad |= 16;
	if ((x * 3)          != 0x0369d0369d0369cdL) bad |= 32;
	/* 1<<40 does not fit a sign-extended imm32 and must not be folded. */
	if ((x + (1L << 40)) != 0x0123466789abcdefL) bad |= 64;
	if (x <= 0x7fffffffL)                        bad |= 128;
	return bad;
}`
	m, err := cc.CompileFor(cc.TargetAMD64, "imm.c", src)
	require.NoError(t, err)
	require.Equal(t, 0, runObj(t, m), "one bit per failing expression, in source order")
}
