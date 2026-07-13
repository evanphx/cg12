package amd64_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// _start stub: call the exported entry `runtest`, then exit(status = its return
// value). Freestanding — no libc — so it runs under qemu-x86_64 with just the
// SYS_exit syscall. The tested backend output is linked alongside it.
const startStub = `
	.text
	.globl _start
_start:
	call runtest
	movl %eax, %edi
	movl $60, %eax
	syscall
`

// runObj compiles m to an x86-64 object, links it (freestanding, static) with the
// _start stub via lld, runs the result under qemu-x86_64, and returns the exit
// code. The module must export a function named "runtest" returning a word.
func runObj(t *testing.T, m *ir.Module) int {
	t.Helper()
	clang, err := exec.LookPath("clang")
	if err != nil {
		t.Skip("clang not available")
	}
	if _, err := exec.LookPath("ld.lld"); err != nil {
		t.Skip("ld.lld not available")
	}
	qemu, err := exec.LookPath("qemu-x86_64")
	if err != nil {
		t.Skip("qemu-x86_64 not available")
	}

	code, err := amd64.CompileObject(m)
	require.NoError(t, err)

	dir := t.TempDir()
	objPath := filepath.Join(dir, "test.o")
	stubS := filepath.Join(dir, "start.s")
	stubO := filepath.Join(dir, "start.o")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(objPath, code, 0o644))
	require.NoError(t, os.WriteFile(stubS, []byte(startStub), 0o644))

	out, err := exec.Command(clang, "--target=x86_64-linux-gnu", "-c", stubS, "-o", stubO).CombinedOutput()
	require.NoErrorf(t, err, "assemble stub: %s", out)

	out, err = exec.Command("ld.lld", "-static", "-nostdlib", "-o", bin, stubO, objPath).CombinedOutput()
	require.NoErrorf(t, err, "link: %s", out)

	cmd := exec.Command(qemu, bin)
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	require.NoError(t, err)
	return 0
}

// entry starts a `runtest` function returning a word.
func entry(m *ir.Module) (*ir.Func, *ir.Block) {
	f := m.NewFunc("runtest", ir.ClsW).Export()
	return f, f.Entry()
}

func TestObjArith(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	// (6*4) + (6-4) == 26; return the difference from 26 (0 on success).
	v := e.Add(ir.ClsW, e.Mul(ir.ClsW, f.Word(6), f.Word(4)), e.Sub(ir.ClsW, f.Word(6), f.Word(4)))
	e.Ret(e.Sub(ir.ClsW, v, f.Word(26)))
	require.Equal(t, 0, runObj(t, m))
}

func TestObjBitwise(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	// ((0xF0 | 0x0F) & 0xFF) ^ 0xAA == 0x55
	x := e.And(ir.ClsW, e.Or(ir.ClsW, f.Word(0xF0), f.Word(0x0F)), f.Word(0xFF))
	v := e.Xor(ir.ClsW, x, f.Word(0xAA))
	e.Ret(e.Sub(ir.ClsW, v, f.Word(0x55)))
	require.Equal(t, 0, runObj(t, m))
}

func TestObjShiftsDivRem(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	// (1<<8)=256; 256>>2=64; 100/7=14; 100%7=2; 14*7+2=100; sum check.
	a := e.Shl(ir.ClsL, f.Long(1), f.Long(8))              // 256
	b := e.Shr(ir.ClsL, a, f.Long(2))                      // 64
	q := e.Div(ir.ClsL, f.Long(100), f.Long(7))            // 14
	r := e.Rem(ir.ClsL, f.Long(100), f.Long(7))            // 2
	chk := e.Add(ir.ClsL, e.Mul(ir.ClsL, q, f.Long(7)), r) // 100
	// b==64 and chk==100 -> return (b-64)+(chk-100) == 0
	v := e.Add(ir.ClsL, e.Sub(ir.ClsL, b, f.Long(64)), e.Sub(ir.ClsL, chk, f.Long(100)))
	e.Ret(e.Extuw(ir.ClsW, v))
	require.Equal(t, 0, runObj(t, m))
}

func TestObjLoopSum(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("runtest", ir.ClsW).Export()
	start := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	exit := f.NewBlock("exit")
	start.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	s := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, f.Word(10)), exit, body)
	s2 := body.Add(ir.ClsW, s, i)
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, i2)
	loop.Phis[1].Add(body, s2)
	exit.Ret(s) // 0+1+..+9 = 45
	require.Equal(t, 45, runObj(t, m))
}

// A helper function called across a relocation, keeping a value live across the
// call (forcing a callee-saved register).
func TestObjCall(t *testing.T) {
	m := ir.NewModule()
	h := m.NewFunc("triple", ir.ClsW)
	x := h.Param("x", ir.ClsW)
	he := h.Entry()
	he.Ret(he.Mul(ir.ClsW, x, h.Word(3)))

	f, e := entry(m)
	// triple(5) + triple(7) == 15 + 21 == 36; but keep first result live across
	// the second call.
	a := e.Call(ir.ClsW, f.Sym("triple", 0), f.Word(5))
	b := e.Call(ir.ClsW, f.Sym("triple", 0), f.Word(7))
	e.Ret(e.Add(ir.ClsW, a, b)) // 36
	require.Equal(t, 36, runObj(t, m))
}

func TestObjFibRecursive(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("fib", ir.ClsW)
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	rec := f.NewBlock("rec")
	base := f.NewBlock("base")
	start.Jnz(start.Cmp(ir.CmpSlt, ir.ClsW, n, f.Word(2)), base, rec)
	base.Ret(n)
	a := rec.Call(ir.ClsW, f.Sym("fib", 0), rec.Sub(ir.ClsW, n, f.Word(1)))
	b := rec.Call(ir.ClsW, f.Sym("fib", 0), rec.Sub(ir.ClsW, n, f.Word(2)))
	rec.Ret(rec.Add(ir.ClsW, a, b))

	rt := m.NewFunc("runtest", ir.ClsW).Export()
	e := rt.Entry()
	e.Ret(e.Call(ir.ClsW, rt.Sym("fib", 0), rt.Word(10))) // fib(10) = 55
	require.Equal(t, 55, runObj(t, m))
}

func TestObjMemory(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	p := e.Alloc(8, 16)
	e.Store(f.Long(0x1122334455667788), p)                            // storel
	e.StoreSub(ir.SubW, f.Word(0xCAFE), e.Add(ir.ClsL, p, f.Long(8))) // storew at p+8
	lo := e.Load(ir.ClsL, p)
	hi := e.Load(ir.ClsW, e.Add(ir.ClsL, p, f.Long(8))) // unsigned 32-bit load
	// lo==0x1122334455667788 and hi==0xCAFE; fold to 0 on success.
	ok1 := e.Sub(ir.ClsL, lo, f.Long(0x1122334455667788))
	ok2 := e.Sub(ir.ClsW, hi, f.Word(0xCAFE))
	e.Ret(e.Add(ir.ClsW, e.Extuw(ir.ClsW, ok1), ok2))
	require.Equal(t, 0, runObj(t, m))
}

func TestObjSubwordExtend(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	p := e.Alloc(8, 8)
	e.StoreSub(ir.SubB, f.Word(0x80), p)
	sb := e.LoadSub(ir.ClsW, ir.SubB, p)  // -128 (signed byte)
	ub := e.LoadSub(ir.ClsW, ir.SubUB, p) // 128
	// sb + ub == 0
	e.Ret(e.Add(ir.ClsW, sb, ub))
	require.Equal(t, 0, runObj(t, m))
}

func TestObjFloat(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	// (2.5 * 4.0 + 1.5) == 11.5; truncate to int 11.
	r := e.Add(ir.ClsD, e.Mul(ir.ClsD, f.Double(2.5), f.Double(4.0)), f.Double(1.5))
	i := e.Stosi(ir.ClsW, r)
	e.Ret(e.Sub(ir.ClsW, i, f.Word(11)))
	require.Equal(t, 0, runObj(t, m))
}

func TestObjFloatConvertRoundtrip(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	// 42 -> double -> single -> double -> int, should be 42.
	d := e.Sltof(ir.ClsD, f.Word(42))
	s := e.Truncd(d)
	d2 := e.Exts(s)
	i := e.Stosi(ir.ClsW, d2)
	e.Ret(e.Sub(ir.ClsW, i, f.Word(42)))
	require.Equal(t, 0, runObj(t, m))
}

func TestObjFloatArgsAndCompare(t *testing.T) {
	m := ir.NewModule()
	// cmp(a,b) returns 1 if a < b else 0, passing doubles as arguments.
	cmp := m.NewFunc("lt", ir.ClsW)
	a := cmp.Param("a", ir.ClsD)
	b := cmp.Param("b", ir.ClsD)
	ce := cmp.Entry()
	ce.Ret(ce.Cmp(ir.CmpFlt, ir.ClsW, a, b)) // result is an integer 0/1

	f, e := entry(m)
	// lt(1.5, 2.5)==1 and lt(2.5,1.5)==0 => 1*10 + 0 == 10.
	r1 := e.Call(ir.ClsW, f.Sym("lt", 0), f.Double(1.5), f.Double(2.5))
	r2 := e.Call(ir.ClsW, f.Sym("lt", 0), f.Double(2.5), f.Double(1.5))
	e.Ret(e.Add(ir.ClsW, e.Mul(ir.ClsW, r1, f.Word(10)), r2)) // 10
	require.Equal(t, 10, runObj(t, m))
}

// A sweep of every integer predicate, packed into a bitmask, checked against the
// expected value for a fixed pair of operands.
func TestObjIntCompareSweep(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	a, b := f.Long(-5), f.Long(3)
	preds := []ir.Cmp{
		ir.CmpEq, ir.CmpNe, ir.CmpSlt, ir.CmpSle, ir.CmpSgt, ir.CmpSge,
		ir.CmpUlt, ir.CmpUle, ir.CmpUgt, ir.CmpUge,
	}
	acc := f.Word(0)
	for i, p := range preds {
		bit := e.Cmp(p, ir.ClsL, a, b)
		acc = e.Or(ir.ClsW, acc, e.Shl(ir.ClsW, bit, f.Word(int64(i))))
	}
	// a=-5, b=3 (signed): ne,slt,sle,ult?  -5 as unsigned is huge > 3.
	// eq=0 ne=1 slt=1 sle=1 sgt=0 sge=0 ult=0 ule=0 ugt=1 uge=1
	// bits: 1<<1 | 1<<2 | 1<<3 | 1<<8 | 1<<9 = 2+4+8+256+512 = 782
	e.Ret(e.Sub(ir.ClsW, acc, f.Word(782)))
	require.Equal(t, 0, runObj(t, m))
}

func TestObjNegation(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	// -(-7) == 7 (int); and -(2.0) truncated == -2, so 7 + (-2) + ... check to 0.
	ni := e.Neg(ir.ClsW, e.Neg(ir.ClsW, f.Word(7))) // 7
	nf := e.Stosi(ir.ClsW, e.Neg(ir.ClsD, f.Double(2.0)))
	// ni==7, nf==-2 => ni + nf == 5; subtract 5.
	e.Ret(e.Sub(ir.ClsW, e.Add(ir.ClsW, ni, nf), f.Word(5)))
	require.Equal(t, 0, runObj(t, m))
}

func TestObjSinglePrecision(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	// (1.5f + 2.5f) * 2.0f == 8.0f -> int 8.
	s := e.Mul(ir.ClsS, e.Add(ir.ClsS, f.Single(1.5), f.Single(2.5)), f.Single(2.0))
	e.Ret(e.Sub(ir.ClsW, e.Stosi(ir.ClsW, s), f.Word(8)))
	require.Equal(t, 0, runObj(t, m))
}

func TestObjDataReference(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{
		Name:  "table",
		Align: 4,
		Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{10, 20, 30, 40}}},
	})
	f, e := entry(m)
	// table[2] + table[3] == 30 + 40 == 70.
	a := e.Load(ir.ClsW, e.Add(ir.ClsL, f.Sym("table", 0), f.Long(8)))
	b := e.Load(ir.ClsW, e.Add(ir.ClsL, f.Sym("table", 0), f.Long(12)))
	e.Ret(e.Add(ir.ClsW, a, b)) // 70
	require.Equal(t, 70, runObj(t, m))
}

func TestObjIndirectCall(t *testing.T) {
	m := ir.NewModule()
	dbl := m.NewFunc("dbl", ir.ClsW)
	x := dbl.Param("x", ir.ClsW)
	de := dbl.Entry()
	de.Ret(de.Mul(ir.ClsW, x, dbl.Word(2)))

	// A function pointer stored in data.
	m.Data = append(m.Data, &ir.Data{
		Name:  "fptr",
		Align: 8,
		Items: []ir.DataItem{{Sym: "dbl"}},
	})
	f, e := entry(m)
	fp := e.Load(ir.ClsL, f.Sym("fptr", 0)) // load the function pointer
	e.Ret(e.Call(ir.ClsW, fp, f.Word(21)))  // (*fp)(21) == 42
	require.Equal(t, 42, runObj(t, m))
}

func TestObjStackArgs(t *testing.T) {
	m := ir.NewModule()
	sum := m.NewFunc("sum8", ir.ClsW)
	var ps []ir.Ref
	for i := 0; i < 8; i++ {
		ps = append(ps, sum.Param(string(rune('a'+i)), ir.ClsW))
	}
	se := sum.Entry()
	acc := ps[0]
	for i := 1; i < 8; i++ {
		acc = se.Add(ir.ClsW, acc, ps[i])
	}
	se.Ret(acc)

	f, e := entry(m)
	var args []ir.Ref
	for i := 1; i <= 8; i++ {
		args = append(args, f.Word(int64(i)))
	}
	// 1+2+...+8 = 36. Two args (7th, 8th) go on the stack.
	e.Ret(e.Call(ir.ClsW, f.Sym("sum8", 0), args...))
	require.Equal(t, 36, runObj(t, m))
}
