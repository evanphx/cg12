package opt_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assembler() (string, bool) {
	if p, err := exec.LookPath("aarch64-linux-gnu-gcc"); err == nil {
		return p, true
	}
	if runtime.GOARCH == "arm64" {
		for _, c := range []string{"cc", "gcc", "clang"} {
			if p, err := exec.LookPath(c); err == nil {
				return p, true
			}
		}
	}
	return "", false
}

func runModule(t *testing.T, m *ir.Module, cmain string) int {
	t.Helper()
	cc, ok := assembler()
	if !ok {
		t.Skip("no AArch64 assembler available")
	}
	o, err := arm64.CompileToObject(m)
	require.NoError(t, err)
	data, err := o.MarshalELF()
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "o.o"), data, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "m.c"), []byte(cmain), 0o644))
	bin := filepath.Join(dir, "p")
	out, err := exec.Command(cc, "-o", bin, filepath.Join(dir, "o.o"), filepath.Join(dir, "m.c")).CombinedOutput()
	require.NoErrorf(t, err, "link failed: %s\n%s", out, arm64.Disassemble(o))
	cmd := exec.Command(bin)
	var sb bytes.Buffer
	cmd.Stdout = &sb
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	require.NoError(t, err)
	return 0
}

func instrCount(f *ir.Func) int {
	n := 0
	for _, b := range f.Blocks {
		n += len(b.Phis) + len(b.Instrs)
	}
	return n
}

func countAdds(f *ir.Func) int { return countOpE2E(f, ir.OAdd) }
func countMuls(f *ir.Func) int { return countOpE2E(f, ir.OMul) }

func countOpE2E(f *ir.Func, op ir.Op) int {
	n := 0
	for _, b := range f.Blocks {
		for _, in := range b.Instrs {
			if in.Op == op {
				n++
			}
		}
	}
	return n
}

// A tangle of constant arithmetic must collapse to a single constant.
func TestOptFoldsWholeExpression(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("konst", ir.ClsW).Export()
	e := f.Entry()
	t1 := e.Mul(ir.ClsW, f.Word(10), f.Word(10)) // 100
	t2 := e.Sub(ir.ClsW, f.Word(7), f.Word(2))   // 5
	t3 := e.Add(ir.ClsW, t1, t2)                 // 105
	e.Ret(e.Shl(ir.ClsW, t3, f.Word(1)))         // 210

	opt.Optimize(f)
	assert.Equal(t, 0, instrCount(f), "everything folds to a constant")

	code := runModule(t, m, `
extern int konst(void);
int main(void){ return konst()==210 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

// A statically-true branch and its dead arm are removed.
func TestOptEliminatesDeadBranch(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("pick", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	start := f.Entry()
	dbl := f.NewBlock("dbl")
	trp := f.NewBlock("trp")
	cond := start.Cmp(ir.CmpSgt, ir.ClsW, f.Word(2), f.Word(1)) // always true
	start.Jnz(cond, dbl, trp)
	dbl.Ret(dbl.Mul(ir.ClsW, x, f.Word(2)))
	trp.Ret(trp.Mul(ir.ClsW, x, f.Word(3)))

	before := len(f.Blocks)
	opt.Optimize(f)
	assert.Less(t, len(f.Blocks), before, "dead arm removed")

	code := runModule(t, m, `
extern int pick(int);
int main(void){ return pick(21)==42 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

// TestOptMem2RegLoop builds a loop whose counter and accumulator live in memory
// (alloc + load/store), promotes them to SSA, and verifies the compiled result.
func TestOptMem2RegLoop(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("sumtomem", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	entry := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	exit := f.NewBlock("exit")

	pi := entry.Alloc(4, 4) // i
	ps := entry.Alloc(4, 4) // s
	entry.Store(f.Word(0), pi)
	entry.Store(f.Word(0), ps)
	entry.Goto(loop)

	iv := loop.Load(ir.ClsW, pi)
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, iv, n), exit, body)

	sv := body.Load(ir.ClsW, ps)
	iv2 := body.Load(ir.ClsW, pi)
	body.Store(body.Add(ir.ClsW, sv, iv2), ps)
	body.Store(body.Add(ir.ClsW, iv2, f.Word(1)), pi)
	body.Goto(loop)

	exit.Ret(exit.Load(ir.ClsW, ps))

	opt.Optimize(f)
	// After promotion no memory ops should remain.
	for _, b := range f.Blocks {
		for _, in := range b.Instrs {
			assert.Falsef(t, in.Op.IsLoad() || in.Op.IsStore() || in.Op.IsAlloc(),
				"memory op %s survived promotion", in.Op)
		}
	}

	code := runModule(t, m, `
extern int sumtomem(int);
int main(void){ return sumtomem(10)==45 && sumtomem(100)==4950 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

// TestOptGVNRedundancy squares a sum: the repeated (a+b) must be computed once,
// and the compiled result must still be correct.
func TestOptGVNRedundancy(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("sqsum", ir.ClsW).Export()
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	e := f.Entry()
	s1 := e.Add(ir.ClsW, a, b)
	s2 := e.Add(ir.ClsW, a, b) // redundant
	e.Ret(e.Mul(ir.ClsW, s1, s2))

	opt.Optimize(f)
	assert.Equal(t, 1, countAdds(f), "the repeated sum is computed once")

	code := runModule(t, m, `
extern int sqsum(int,int);
int main(void){ return sqsum(3,4)==49 && sqsum(-2,5)==9 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

// TestOptGCMLoopInvariant hoists a loop-invariant multiply out of the loop and
// confirms the compiled program still computes the right sum.
func TestOptGCMLoopInvariant(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("acc", ir.ClsW).Export()
	a := f.Param("a", ir.ClsW)
	b := f.Param("b", ir.ClsW)
	n := f.Param("n", ir.ClsW)
	entry := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	exit := f.NewBlock("exit")

	entry.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: entry, Val: f.Word(0)})
	s := loop.Phi(ir.ClsW, ir.PhiEdge{From: entry, Val: f.Word(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), exit, body)
	inv := body.Mul(ir.ClsW, a, b) // loop-invariant
	s2 := body.Add(ir.ClsW, body.Add(ir.ClsW, s, inv), i)
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, i2)
	loop.Phis[1].Add(body, s2)
	exit.Ret(s)

	opt.Optimize(f)
	assert.Equal(t, 1, countMuls(f), "the invariant multiply appears once")

	// sum_{i=0}^{n-1} (a*b + i) = n*a*b + n(n-1)/2
	code := runModule(t, m, `
extern int acc(int,int,int);
int main(void){
  int a=3,b=4,n=5; int want = n*a*b + n*(n-1)/2;   // 70
  return acc(a,b,n)==want ? 0 : 1;
}`)
	assert.Equal(t, 0, code)
}

func TestOptLoadForwarding(t *testing.T) {
	// Redundant loads through a pointer collapse to one; result unchanged.
	m := ir.NewModule()
	f := m.NewFunc("twice", ir.ClsW).Export()
	p := f.Param("p", ir.ClsL)
	e := f.Entry()
	a := e.Load(ir.ClsW, p)
	b := e.Load(ir.ClsW, p)
	e.Ret(e.Add(ir.ClsW, a, b))

	opt.Optimize(f)
	assert.Equal(t, 1, countOpE2E(f, ir.OLoaduw), "one of the two loads eliminated")

	code := runModule(t, m, `
extern int twice(int*);
int main(void){ int x=21; return twice(&x)==42 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestOptStoreForwarding(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("st", ir.ClsW).Export()
	p := f.Param("p", ir.ClsL)
	e := f.Entry()
	e.Store(f.Word(42), p)
	e.Ret(e.Load(ir.ClsW, p))

	opt.Optimize(f)
	assert.Equal(t, 0, countOpE2E(f, ir.OLoaduw), "load forwarded from the store")

	code := runModule(t, m, `
extern int st(int*);
int main(void){ int x=0; int r=st(&x); return (r==42 && x==42) ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

// TestOptLoadElimSoundness is the important one: two possibly-aliasing pointers
// must NOT forward the first store. Called with p == q, the correct answer is
// the second store's value.
func TestOptLoadElimSoundness(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("alias", ir.ClsW).Export()
	p := f.Param("p", ir.ClsL)
	q := f.Param("q", ir.ClsL)
	e := f.Entry()
	e.Store(f.Word(1), p)
	e.Store(f.Word(2), q)
	e.Ret(e.Load(ir.ClsW, p)) // if wrongly forwarded, returns 1

	opt.Optimize(f)

	code := runModule(t, m, `
extern int alias(int*,int*);
int main(void){ int x=0; return alias(&x,&x)==2 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

// Optimization must preserve semantics of a non-trivial function: a loop with a
// constant subexpression and a dead instruction, run before and after.
func TestOptPreservesLoopSemantics(t *testing.T) {
	build := func() (*ir.Module, *ir.Func) {
		m := ir.NewModule()
		f := m.NewFunc("sumto", ir.ClsW).Export()
		n := f.Param("n", ir.ClsW)
		start := f.Entry()
		loop := f.NewBlock("loop")
		body := f.NewBlock("body")
		exit := f.NewBlock("exit")
		start.Add(ir.ClsW, f.Word(2), f.Word(3)) // dead constant subexpression
		start.Goto(loop)
		i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
		s := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
		loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), exit, body)
		s2 := body.Add(ir.ClsW, s, i)
		i2 := body.Add(ir.ClsW, i, f.Word(1))
		body.Goto(loop)
		loop.Phis[0].Add(body, i2)
		loop.Phis[1].Add(body, s2)
		exit.Ret(s)
		return m, f
	}

	// Unoptimized reference.
	mRef, _ := build()
	refCode := runModule(t, mRef, `extern int sumto(int); int main(void){ return sumto(15)==105?0:1; }`)
	assert.Equal(t, 0, refCode)

	// Optimized: dead constant add is gone, result identical.
	mOpt, fOpt := build()
	before := instrCount(fOpt)
	opt.Optimize(fOpt)
	assert.Less(t, instrCount(fOpt), before, "dead code removed")

	optCode := runModule(t, mOpt, `extern int sumto(int); int main(void){ return sumto(15)==105?0:1; }`)
	assert.Equal(t, 0, optCode)
}
