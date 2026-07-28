package amd64_test

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/amd64/x64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// The bit-manipulation ops: ir.OClz and the high half of a widening multiply
// (ir.OUMulh / ir.OSMulh). Each is covered twice over, because the two layers
// catch different mistakes.
//
// The encoding tests assert the exact byte sequence the selector emits, so a
// sequence that is wrong in a way the hardware cannot reveal -- an operand-size
// prefix on the wrong instruction, a conditional move on the wrong flag, the
// correction applied in an order that clobbers the flags it depends on -- is
// named at its source rather than showing up as an arbitrary wrong number.
//
// The execution tests then run the emitted code natively and compare against
// values Go itself computed (bits.LeadingZeros*, bits.Mul64, math/big for the
// signed product). Deriving the expectations that way rather than writing them
// out is what makes the interesting cases worth having: the high half of a
// multiply is zero for every small operand, so a test built from small numbers
// passes against an implementation that returns nothing at all.

// --- encoding ---------------------------------------------------------------

// bitsFuncText compiles m and returns the machine code of one function, so a
// test can assert on the bytes for a single operation without reaching through an
// ELF file.
func bitsFuncText(t *testing.T, m *ir.Module, name string) []byte {
	t.Helper()
	o, err := amd64.CompileToObject(m)
	require.NoError(t, err)
	for _, s := range o.Syms {
		if s.Name == name && s.Func {
			return o.Text[s.Value : s.Value+s.Size]
		}
	}
	t.Fatalf("no function symbol %q in the compiled object", name)
	return nil
}

// bitsRequireSeq asserts that code contains want, the concatenation of the
// instructions an operation should have expanded to. It is a containment check
// rather than an equality one so that the prologue and epilogue surrounding the
// sequence -- nobody's business here, and owned elsewhere -- can change freely.
func bitsRequireSeq(t *testing.T, code []byte, want [][]byte, what string) {
	t.Helper()
	var seq []byte
	for _, in := range want {
		seq = append(seq, in...)
	}
	require.Truef(t, bytes.Contains(code, seq),
		"%s: expected instruction sequence % x\nwithin emitted code       % x", what, seq, code)
}

// bitsOneArgFunc builds a module holding a single exported one-parameter
// function, the smallest thing whose text can be read back. The parameter is a
// real parameter rather than a constant so the operand genuinely arrives in a
// register.
func bitsOneArgFunc(name string, cls ir.Cls, body func(b *ir.Block, x ir.Ref) ir.Ref) *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc(name, cls).Export()
	x := f.Param("x", cls)
	e := f.Entry()
	e.Ret(body(e, x))
	return m
}

// bitsTwoArgFunc is bitsOneArgFunc for a binary operation.
func bitsTwoArgFunc(name string, cls ir.Cls, body func(b *ir.Block, x, y ir.Ref) ir.Ref) *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc(name, cls).Export()
	x := f.Param("x", cls)
	y := f.Param("y", cls)
	e := f.Entry()
	e.Ret(body(e, x, y))
	return m
}

// The System V argument registers the two helpers' parameters arrive in, and the
// first allocatable register the result lands in (reg.go's intAllocOrder starts at
// RSI). Spelling them out keeps the expected sequences below readable.
const (
	bitsArg0Reg = x64.RDI
	bitsArg1Reg = x64.RSI
	bitsDstReg  = x64.RSI
)

func TestClzEncoding(t *testing.T) {
	// 64-bit: bsr gives the index of the top set bit and sets ZF when the source
	// is zero; the CMOVE substitutes -1 for the (architecturally undefined)
	// index in that case, and 63 - (-1) is the 64 that OClz of zero must be. The
	// CMOVE has to precede the SUB, which writes flags.
	code := bitsFuncText(t, bitsOneArgFunc("clz64", ir.ClsL, func(b *ir.Block, x ir.Ref) ir.Ref {
		return b.Clz(ir.ClsL, x)
	}), "clz64")
	bitsRequireSeq(t, code, [][]byte{
		x64.Bsr(true, x64.RCX, bitsArg0Reg),
		x64.MovImm32(true, x64.RDX, -1),
		x64.Cmovcc(true, x64.E, x64.RCX, x64.RDX),
		x64.MovImm32(true, bitsDstReg, 63),
		x64.SubReg(true, bitsDstReg, x64.RCX),
	}, "clz of a 64-bit operand")

	// 32-bit: the same shape at the narrower operand size, and 31 rather than 63.
	// Every instruction has to narrow together -- a 64-bit bsr here would read
	// whatever happens to be in the upper half of the source register.
	code = bitsFuncText(t, bitsOneArgFunc("clz32", ir.ClsW, func(b *ir.Block, x ir.Ref) ir.Ref {
		return b.Clz(ir.ClsW, x)
	}), "clz32")
	bitsRequireSeq(t, code, [][]byte{
		x64.Bsr(false, x64.RCX, bitsArg0Reg),
		x64.MovImm32(false, x64.RDX, -1),
		x64.Cmovcc(false, x64.E, x64.RCX, x64.RDX),
		x64.MovImm32(false, bitsDstReg, 31),
		x64.SubReg(false, bitsDstReg, x64.RCX),
	}, "clz of a 32-bit operand")
}

func TestMulhEncoding(t *testing.T) {
	// The multiplicand goes to RAX because the one-operand MUL is written that
	// way; the high half comes back out of RDX. Both registers are held out of
	// allocation for exactly this, so no shuffling is needed around them.
	code := bitsFuncText(t, bitsTwoArgFunc("umulh", ir.ClsL, func(b *ir.Block, x, y ir.Ref) ir.Ref {
		return b.UMulh(x, y)
	}), "umulh")
	bitsRequireSeq(t, code, [][]byte{
		x64.MovReg(true, x64.RAX, bitsArg0Reg),
		x64.MulWide(true, bitsArg1Reg),
		x64.MovReg(true, bitsDstReg, x64.RDX),
	}, "unsigned high-half multiply")

	// Signed differs by exactly one opcode extension field (F7 /5 rather than
	// /4), which is the whole point: the low half is identical between the two and
	// the high half is where the sign shows up.
	code = bitsFuncText(t, bitsTwoArgFunc("smulh", ir.ClsL, func(b *ir.Block, x, y ir.Ref) ir.Ref {
		return b.SMulh(x, y)
	}), "smulh")
	bitsRequireSeq(t, code, [][]byte{
		x64.MovReg(true, x64.RAX, bitsArg0Reg),
		x64.ImulWide(true, bitsArg1Reg),
		x64.MovReg(true, bitsDstReg, x64.RDX),
	}, "signed high-half multiply")
}

// TestClzUsesNoFeatureGatedEncoding guards the decision in x64/bits.go by
// checking what is *absent*: LZCNT is BSR with an F3 prefix, and a CPU without
// the feature ignores the prefix and executes the bit scan, so a stray LZCNT
// would compile, link, and run -- returning a different number on older hardware
// only. cg12 has no target feature model to gate it on, so the byte pair must not
// appear ahead of a bit scan.
func TestClzUsesNoFeatureGatedEncoding(t *testing.T) {
	for _, cls := range []ir.Cls{ir.ClsW, ir.ClsL} {
		code := bitsFuncText(t, bitsOneArgFunc("clz", cls, func(b *ir.Block, x ir.Ref) ir.Ref {
			return b.Clz(cls, x)
		}), "clz")
		require.NotContainsf(t, string(code), "\xf3\x0f\xbd", "lzcnt (%s) needs a CPU feature cg12 cannot check for", cls)
		require.NotContainsf(t, string(code), "\xf3\x0f\xbc", "tzcnt (%s) needs a CPU feature cg12 cannot check for", cls)
	}
}

// --- execution --------------------------------------------------------------

// runBitCases compiles and runs a module built around one operation, evaluated
// in a helper function `op` (nargs parameters of class cls, one result of the
// same class) rather than inline, so each operand genuinely arrives in a register
// at run time instead of being folded into an immediate operand.
//
// `runtest` calls op once per case and folds one bit per mismatching case into
// its return value, so the exit code is a bitmask naming exactly which cases
// went wrong -- 0 when every one agrees. That is the same shape convert_test.go
// uses, and for the same reason: a single pass/fail bit would not say which value
// broke.
func runBitCases(t *testing.T, cls ir.Cls, nargs int, body func(b *ir.Block, args []ir.Ref) ir.Ref, cases func(f *ir.Func) (args [][]ir.Ref, wants []ir.Ref)) int {
	t.Helper()
	m := ir.NewModule()

	op := m.NewFunc("op", cls)
	var params []ir.Ref
	for i := 0; i < nargs; i++ {
		params = append(params, op.Param(fmt.Sprintf("a%d", i), cls))
	}
	ob := op.Entry()
	ob.Ret(body(ob, params))

	f, e := entry(m)
	args, wants := cases(f)
	require.Equal(t, len(args), len(wants), "one expected value per case")
	require.LessOrEqual(t, len(args), 8, "the exit code carries one bit per case")

	acc := f.Word(0)
	for i := range args {
		require.Equal(t, nargs, len(args[i]), "case %d has the wrong arity", i)
		got := e.Call(cls, f.Sym("op", 0), args[i]...)
		mismatch := e.Cmp(ir.CmpNe, ir.ClsW, got, wants[i])
		acc = e.Or(ir.ClsW, acc, e.Shl(ir.ClsW, mismatch, f.Word(int64(i))))
	}
	e.Ret(acc)
	return runObj(t, m)
}

// clz64Values span the cases the BSR correction has to get right: zero, where the
// instruction's own result is undefined and the answer comes from the flags; one
// and the top bit, the two ends of the range; and a value with every bit set.
var clz64Values = []uint64{
	0,
	1,
	2,
	0xff,
	1 << 32,
	0xffffffff,
	1 << 63,
	math.MaxUint64,
}

func TestObjClz64(t *testing.T) {
	code := runBitCases(t, ir.ClsL, 1,
		func(b *ir.Block, args []ir.Ref) ir.Ref { return b.Clz(ir.ClsL, args[0]) },
		func(f *ir.Func) (args [][]ir.Ref, wants []ir.Ref) {
			for _, v := range clz64Values {
				args = append(args, []ir.Ref{f.Long(int64(v))})
				wants = append(wants, f.Long(int64(bits.LeadingZeros64(v))))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "mismatching cases (bit i = clz64Values[i])")
}

// clz32Values are the 32-bit equivalents. The narrow width needs its own coverage
// because every instruction in the sequence changes operand size with it, and the
// constant subtracted from changes from 63 to 31.
var clz32Values = []uint32{
	0,
	1,
	2,
	0xff,
	0xffff,
	1 << 30,
	1 << 31,
	math.MaxUint32,
}

func TestObjClz32(t *testing.T) {
	code := runBitCases(t, ir.ClsW, 1,
		func(b *ir.Block, args []ir.Ref) ir.Ref { return b.Clz(ir.ClsW, args[0]) },
		func(f *ir.Func) (args [][]ir.Ref, wants []ir.Ref) {
			for _, v := range clz32Values {
				args = append(args, []ir.Ref{f.Word(int64(int32(v)))})
				wants = append(wants, f.Word(int64(bits.LeadingZeros32(v))))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "mismatching cases (bit i = clz32Values[i])")
}

// umulhPairs are chosen so the high half is non-zero in most of them. Operands
// small enough to make the high half zero prove nothing -- an implementation that
// returned a constant zero, or read the low half out of RAX, would pass -- so the
// list is anchored at MaxUint64 * MaxUint64 and at the powers of two that carry a
// single bit across the word boundary.
var umulhPairs = [][2]uint64{
	{3, 5},                                   // high half genuinely zero
	{math.MaxUint64, 1},                      // still zero, at the top of the low half
	{math.MaxUint64, 2},                      // the first case that carries
	{1 << 32, 1 << 32},                       // exactly one bit into the high half
	{1 << 63, 2},                             //
	{math.MaxUint64, math.MaxUint64},         // the largest product representable
	{0xdeadbeefdeadbeef, 0xcafebabecafebabe}, //
	{0x0123456789abcdef, 0x0fedcba987654321}, //
}

func TestObjUMulh(t *testing.T) {
	code := runBitCases(t, ir.ClsL, 2,
		func(b *ir.Block, args []ir.Ref) ir.Ref { return b.UMulh(args[0], args[1]) },
		func(f *ir.Func) (args [][]ir.Ref, wants []ir.Ref) {
			for _, p := range umulhPairs {
				hi, _ := bits.Mul64(p[0], p[1])
				args = append(args, []ir.Ref{f.Long(int64(p[0])), f.Long(int64(p[1]))})
				wants = append(wants, f.Long(int64(hi)))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "mismatching cases (bit i = umulhPairs[i])")
}

// smulhHigh returns the high 64 bits of the signed 128-bit product of a and b,
// computed with math/big rather than derived from the unsigned high half, so the
// expectation does not share an algorithm with the thing under test. big.Int's
// right shift is arithmetic, which is what makes >>64 the two's-complement high
// word for a negative product too.
func smulhHigh(a, b int64) int64 {
	p := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	return new(big.Int).Rsh(p, 64).Int64()
}

// smulhPairs cover both signs in both operands, plus the two extremes where the
// signed and unsigned high halves differ most.
var smulhPairs = [][2]int64{
	{3, 5},                         // positive, high half zero
	{-5, 7},                        // mixed signs: high half is all ones
	{-5, -7},                       // both negative: product positive
	{math.MaxInt64, math.MaxInt64}, //
	{math.MinInt64, math.MinInt64}, // the only product that reaches 2^126
	{math.MinInt64, -1},            // negating the most negative value
	{math.MaxInt64, math.MinInt64}, //
	{0x0123456789abcdef, -0x0fedcba987654321}, //
}

func TestObjSMulh(t *testing.T) {
	code := runBitCases(t, ir.ClsL, 2,
		func(b *ir.Block, args []ir.Ref) ir.Ref { return b.SMulh(args[0], args[1]) },
		func(f *ir.Func) (args [][]ir.Ref, wants []ir.Ref) {
			for _, p := range smulhPairs {
				args = append(args, []ir.Ref{f.Long(p[0]), f.Long(p[1])})
				wants = append(wants, f.Long(smulhHigh(p[0], p[1])))
			}
			return args, wants
		})
	require.Equal(t, 0, code, "mismatching cases (bit i = smulhPairs[i])")
}

// TestObjSignedVersusUnsignedMulh checks the one thing the two lists above cannot
// individually: that the signed and unsigned forms are not the same instruction
// under two names. Both are asked for the same pair of operands, whose signed and
// unsigned high halves differ, and the results are required to differ accordingly.
func TestObjSignedVersusUnsignedMulh(t *testing.T) {
	a, b := int64(-3), int64(5)
	uhi, _ := bits.Mul64(uint64(a), uint64(b))
	shi := smulhHigh(a, b)
	require.NotEqual(t, int64(uhi), shi, "the operands must distinguish the two forms")

	m := ir.NewModule()
	op := m.NewFunc("op", ir.ClsL)
	x := op.Param("x", ir.ClsL)
	y := op.Param("y", ir.ClsL)
	ob := op.Entry()
	// Return 0 only when both halves are what Go says they are.
	bad := ob.Or(ir.ClsL,
		ob.Cmp(ir.CmpNe, ir.ClsL, ob.UMulh(x, y), op.Long(int64(uhi))),
		ob.Shl(ir.ClsL, ob.Cmp(ir.CmpNe, ir.ClsL, ob.SMulh(x, y), op.Long(shi)), op.Long(1)))
	ob.Ret(bad)

	f, e := entry(m)
	e.Ret(e.Extuw(ir.ClsW, e.Call(ir.ClsL, f.Sym("op", 0), f.Long(a), f.Long(b))))
	require.Equal(t, 0, runObj(t, m), "bit 0 = unsigned high half wrong, bit 1 = signed")
}

// TestObjClzFromCBuiltins goes through the frontend that actually produces
// ir.OClz today, rather than building the op by hand: __builtin_clz{,ll} lower to
// it directly, and __builtin_ctz{,ll} lower to the count of leading zeros of the
// isolated low bit (cc/builtin.go). Both families failed to compile on amd64
// before this, with "unsupported op clz", so this is the end-to-end check that
// the live consumer now works: the frontend's own sequence around the op, at both
// widths, rather than the op in isolation. No zero argument appears, because C
// leaves both builtins undefined there -- the boundary cases that are defined are
// a single bit at either end of the word.
func TestObjClzFromCBuiltins(t *testing.T) {
	src := `
int runtest(void) {
	int bad = 0;
	if (__builtin_clz(1u) != 31)                  bad |= 1;
	if (__builtin_clz(0x80000000u) != 0)          bad |= 2;
	if (__builtin_clz(0xffffu) != 16)             bad |= 4;
	if (__builtin_clzll(1ull) != 63)              bad |= 8;
	if (__builtin_clzll(1ull << 63) != 0)         bad |= 16;
	if (__builtin_ctz(8u) != 3)                   bad |= 32;
	if (__builtin_ctzll(1ull << 40) != 40)        bad |= 64;
	if (__builtin_ctzll(0xffffffffffffffffull))   bad |= 128;
	return bad;
}`
	m, err := cc.CompileFor(cc.TargetAMD64, "clz.c", src)
	require.NoError(t, err)
	require.Equal(t, 0, runObj(t, m), "one bit per failing builtin, in source order")
}

// TestObjBitsUnderRegisterPressure runs the same operations with their operands
// and results in spill slots rather than registers, which is a different code path
// on both ends: the operand is reloaded into a scratch register first, and the
// result is stored back out of one. Ten simultaneously-live values against nine
// allocatable GPRs guarantees it, and summing them in reverse order of definition
// is what keeps them all live at once.
func TestObjBitsUnderRegisterPressure(t *testing.T) {
	const seed = uint64(0x0123456789abcdef)

	m := ir.NewModule()
	op := m.NewFunc("op", ir.ClsL)
	x := op.Param("x", ir.ClsL)
	ob := op.Entry()

	var vals []ir.Ref
	want := int64(0)
	for i := 0; i < 10; i++ {
		shift := int64(i * 6)
		vals = append(vals, ob.Clz(ir.ClsL, ob.Shr(ir.ClsL, x, op.Long(shift))))
		want += int64(bits.LeadingZeros64(seed >> uint(shift)))
	}
	// One high-half multiply in the middle of the pressure, so its RAX/RDX
	// staging is exercised with a spilled operand too.
	hi, _ := bits.Mul64(seed, seed)
	vals = append(vals, ob.UMulh(x, x))
	want += int64(hi)

	sum := op.Long(0)
	for i := len(vals) - 1; i >= 0; i-- {
		sum = ob.Add(ir.ClsL, sum, vals[i])
	}
	ob.Ret(sum)

	f, e := entry(m)
	got := e.Call(ir.ClsL, f.Sym("op", 0), f.Long(int64(seed)))
	e.Ret(e.Cmp(ir.CmpNe, ir.ClsW, got, f.Long(want)))
	require.Equal(t, 0, runObj(t, m))
}
