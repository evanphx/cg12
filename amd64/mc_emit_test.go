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

// runText renders m to assembly text, assembles it with clang, links it
// freestanding with the _start stub, runs it under qemu-x86_64, and returns the
// exit code. It validates the text emitter's output end-to-end.
func runText(t *testing.T, m *ir.Module) int {
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

	asm, err := amd64.CompileModule(m)
	require.NoError(t, err)

	dir := t.TempDir()
	asmPath := filepath.Join(dir, "test.s")
	objPath := filepath.Join(dir, "test.o")
	stubS := filepath.Join(dir, "start.s")
	stubO := filepath.Join(dir, "start.o")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(asmPath, []byte(asm), 0o644))
	require.NoError(t, os.WriteFile(stubS, []byte(startStub), 0o644))

	out, err := exec.Command(clang, "--target=x86_64-linux-gnu", "-c", asmPath, "-o", objPath).CombinedOutput()
	require.NoErrorf(t, err, "assemble emitted text failed: %s\n---\n%s", out, asm)
	out, err = exec.Command(clang, "--target=x86_64-linux-gnu", "-c", stubS, "-o", stubO).CombinedOutput()
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

func TestTextArith(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	v := e.Add(ir.ClsW, e.Mul(ir.ClsW, f.Word(6), f.Word(4)), e.Sub(ir.ClsW, f.Word(6), f.Word(4)))
	e.Ret(e.Sub(ir.ClsW, v, f.Word(26)))
	require.Equal(t, 0, runText(t, m))
}

func TestTextLoopSum(t *testing.T) {
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
	exit.Ret(s)
	require.Equal(t, 45, runText(t, m))
}

func TestTextCallAndDivRem(t *testing.T) {
	m := ir.NewModule()
	h := m.NewFunc("triple", ir.ClsW)
	x := h.Param("x", ir.ClsW)
	he := h.Entry()
	he.Ret(he.Mul(ir.ClsW, x, h.Word(3)))

	f, e := entry(m)
	a := e.Call(ir.ClsW, f.Sym("triple", 0), f.Word(5)) // 15
	b := e.Call(ir.ClsW, f.Sym("triple", 0), f.Word(7)) // 21
	q := e.Div(ir.ClsL, f.Long(100), f.Long(7))         // 14
	r := e.Rem(ir.ClsL, f.Long(100), f.Long(7))         // 2
	// (15 + 21) + (14 - 14) + (2 - 2) == 36
	sum := e.Add(ir.ClsW, a, b)
	sum = e.Add(ir.ClsW, sum, e.Extuw(ir.ClsW, e.Sub(ir.ClsL, q, f.Long(14))))
	sum = e.Add(ir.ClsW, sum, e.Extuw(ir.ClsW, e.Sub(ir.ClsL, r, f.Long(2))))
	e.Ret(e.Sub(ir.ClsW, sum, f.Word(36)))
	require.Equal(t, 0, runText(t, m))
}

func TestTextFloat(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	r := e.Add(ir.ClsD, e.Mul(ir.ClsD, f.Double(2.5), f.Double(4.0)), f.Double(1.5))
	e.Ret(e.Sub(ir.ClsW, e.Stosi(ir.ClsW, r), f.Word(11)))
	require.Equal(t, 0, runText(t, m))
}

func TestTextMemory(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	p := e.Alloc(8, 16)
	e.Store(f.Long(0x1122334455667788), p)
	e.StoreSub(ir.SubW, f.Word(0xCAFE), e.Add(ir.ClsL, p, f.Long(8)))
	lo := e.Load(ir.ClsL, p)
	hi := e.Load(ir.ClsW, e.Add(ir.ClsL, p, f.Long(8)))
	ok1 := e.Sub(ir.ClsL, lo, f.Long(0x1122334455667788))
	ok2 := e.Sub(ir.ClsW, hi, f.Word(0xCAFE))
	e.Ret(e.Add(ir.ClsW, e.Extuw(ir.ClsW, ok1), ok2))
	require.Equal(t, 0, runText(t, m))
}

func TestTextStackArgs(t *testing.T) {
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
	e.Ret(e.Call(ir.ClsW, f.Sym("sum8", 0), args...))
	require.Equal(t, 36, runText(t, m))
}

func TestTextVariadic(t *testing.T) {
	m := ir.NewModule()
	vsum := m.NewFunc("vsum", ir.ClsW)
	vsum.Variadic = true
	vsum.Param("n", ir.ClsW)
	ve := vsum.Entry()
	ap := ve.Alloc(8, 32)
	ve.VaStart(ap)
	acc := ve.VaArg(ir.ClsW, ap)
	acc = ve.Add(ir.ClsW, acc, ve.VaArg(ir.ClsW, ap))
	acc = ve.Add(ir.ClsW, acc, ve.VaArg(ir.ClsW, ap))
	ve.Ret(acc)

	f, e := entry(m)
	r := e.Call(ir.ClsW, f.Sym("vsum", 0), f.Word(3), f.Word(11), f.Word(22), f.Word(33))
	e.Ret(e.Sub(ir.ClsW, r, f.Word(66)))
	require.Equal(t, 0, runText(t, m))
}

func TestTextAggregate(t *testing.T) {
	m := ir.NewModule()
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m.AddType(pair)

	// mk(a,b) returns {a,b}; sum2(p) returns lo+hi.
	mk := m.NewFuncVoid("mk")
	mk.HasRet = true
	mk.RetAgg = pair
	a := mk.Param("a", ir.ClsW)
	b := mk.Param("b", ir.ClsW)
	me := mk.Entry()
	buf := me.Alloc(8, 8)
	me.StoreSub(ir.SubW, a, buf)
	me.StoreSub(ir.SubW, b, me.Add(ir.ClsL, buf, mk.Long(4)))
	me.Ret(buf)

	sum2 := m.NewFunc("sum2", ir.ClsW)
	p := aggParam(sum2, "p", pair)
	se := sum2.Entry()
	se.Ret(se.Add(ir.ClsW, se.Load(ir.ClsW, p), se.Load(ir.ClsW, se.Add(ir.ClsL, p, sum2.Long(4)))))

	f, e := entry(m)
	s := e.Call(ir.ClsL, f.Sym("mk", 0), f.Word(7), f.Word(35))
	e.Instrs[len(e.Instrs)-1].RetAgg = pair
	r := e.Call(ir.ClsW, f.Sym("sum2", 0), s)
	e.Instrs[len(e.Instrs)-1].AggArgs = []*ir.AggType{pair}
	e.Ret(e.Sub(ir.ClsW, r, f.Word(42)))
	require.Equal(t, 0, runText(t, m))
}

// A module carrying source positions emits .file/.loc directives that the
// assembler accepts and turns into a line table.
func TestTextWithPositions(t *testing.T) {
	m := ir.NewModule()
	file := m.File("prog.src")
	f := m.NewFunc("runtest", ir.ClsW).Export()
	e := f.Entry()
	e.At(ir.SrcPos{File: file, Line: 3, Col: 1})
	s := e.Add(ir.ClsW, f.Word(20), f.Word(22))
	e.At(ir.SrcPos{File: file, Line: 4, Col: 1})
	e.Ret(e.Sub(ir.ClsW, s, f.Word(42))) // 20 + 22 - 42 == 0
	require.Equal(t, 0, runText(t, m))
}

// Compile renders a single function to assembly text with the expected shape.
func TestTextCompileSingle(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("addfn", ir.ClsW).Export()
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	f.Entry().Ret(f.Entry().Add(ir.ClsW, a, b))
	asm, err := amd64.Compile(f)
	require.NoError(t, err)
	require.Contains(t, asm, ".globl addfn")
	require.Contains(t, asm, "pushq %rbp")
	require.Contains(t, asm, "addl")
	require.Contains(t, asm, "ret")
}

func TestTextDataReference(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{
		Name:  "table",
		Align: 4,
		Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{10, 20, 30, 40}}},
	})
	f, e := entry(m)
	a := e.Load(ir.ClsW, e.Add(ir.ClsL, f.Sym("table", 0), f.Long(8)))
	b := e.Load(ir.ClsW, e.Add(ir.ClsL, f.Sym("table", 0), f.Long(12)))
	e.Ret(e.Add(ir.ClsW, a, b))
	require.Equal(t, 70, runText(t, m))
}
