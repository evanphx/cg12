package arm64_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// assembler finds a C compiler that produces native AArch64 objects, or skips
// -- unless CG12_REQUIRE_TOOLS says a run without one is a failure rather than a
// pass that checked nothing. On a host that is not arm64 and has no cross
// compiler there is no tool to demand, so that stays a plain skip.
func assembler(t testing.TB) string {
	t.Helper()
	if p, ok := testenv.Have("aarch64-linux-gnu-gcc"); ok {
		return p
	}
	if runtime.GOARCH != "arm64" {
		t.Skip("no AArch64 toolchain: this host is not arm64 and there is no cross compiler")
	}
	return testenv.Tool(t, "cc", "gcc", "clang")
}

// buildAndRun compiles m to an object, links it with a C driver, runs the
// program, and returns its stdout and exit code. It skips when no AArch64
// toolchain is available so the suite stays portable.
func buildAndRun(t *testing.T, m *ir.Module, cmain string) (string, int) {
	t.Helper()
	cc := assembler(t)
	o, err := arm64.CompileToObject(m)
	require.NoError(t, err)
	data, err := o.MarshalELF()
	require.NoError(t, err)

	dir := t.TempDir()
	objPath := filepath.Join(dir, "out.o")
	cPath := filepath.Join(dir, "main.c")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(objPath, data, 0o644))
	require.NoError(t, os.WriteFile(cPath, []byte(cmain), 0o644))

	out, err := exec.Command(cc, "-o", bin, objPath, cPath).CombinedOutput()
	require.NoErrorf(t, err, "link failed: %s\n--- asm ---\n%s", out, arm64.Disassemble(o))

	cmd := exec.Command(bin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run: %v", err)
	}
	return stdout.String(), code
}

func TestE2EAdd(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("add", ir.ClsW).Export()
	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, a, b))

	_, code := buildAndRun(t, m, `
extern int add(int,int);
int main(void){ return add(3,4)==7 && add(-5,5)==0 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestE2ESumtoLoop(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("sumto", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	exit := f.NewBlock("exit")
	start.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	s := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	c := loop.Cmp(ir.CmpSge, ir.ClsW, i, n)
	loop.Jnz(c, exit, body)
	s2 := body.Add(ir.ClsW, s, i)
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, i2)
	loop.Phis[1].Add(body, s2)
	exit.Ret(s)

	_, code := buildAndRun(t, m, `
extern int sumto(int);
int main(void){ return sumto(10)==45 && sumto(100)==4950 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestE2EFibRecursive(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("fib", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	rec := f.NewBlock("rec")
	base := f.NewBlock("base")
	start.Jnz(start.Cmp(ir.CmpSlt, ir.ClsW, n, f.Word(2)), base, rec)
	base.Ret(n)
	a := rec.Call(ir.ClsW, f.Sym("fib", 0), rec.Sub(ir.ClsW, n, f.Word(1)))
	b := rec.Call(ir.ClsW, f.Sym("fib", 0), rec.Sub(ir.ClsW, n, f.Word(2)))
	rec.Ret(rec.Add(ir.ClsW, a, b))

	_, code := buildAndRun(t, m, `
extern int fib(int);
int main(void){ return fib(10)==55 && fib(15)==610 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// TestE2EArithmetic exercises the full integer op set and checks it against a C
// reimplementation over several signed inputs.
func TestE2EArithmetic(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("mix", ir.ClsW).Export()
	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
	e := f.Entry()
	acc := e.Add(ir.ClsW, a, b)
	acc = e.Add(ir.ClsW, acc, e.Sub(ir.ClsW, a, b))
	acc = e.Add(ir.ClsW, acc, e.Mul(ir.ClsW, a, b))
	acc = e.Add(ir.ClsW, acc, e.Div(ir.ClsW, a, b))
	acc = e.Add(ir.ClsW, acc, e.Rem(ir.ClsW, a, b))
	acc = e.Add(ir.ClsW, acc, e.And(ir.ClsW, a, b))
	acc = e.Add(ir.ClsW, acc, e.Or(ir.ClsW, a, b))
	acc = e.Add(ir.ClsW, acc, e.Xor(ir.ClsW, a, b))
	acc = e.Add(ir.ClsW, acc, e.Shl(ir.ClsW, a, f.Word(3)))
	acc = e.Add(ir.ClsW, acc, e.Sar(ir.ClsW, a, f.Word(1)))
	acc = e.Add(ir.ClsW, acc, e.Neg(ir.ClsW, a))
	e.Ret(acc)

	_, code := buildAndRun(t, m, `
extern int mix(int,int);
static int cmix(int a,int b){
  return (a+b)+(a-b)+(a*b)+(a/b)+(a%b)+(a&b)+(a|b)+(a^b)+(a<<3)+(a>>1)+(-a);
}
int main(void){
  int pairs[][2]={{20,7},{-20,7},{20,-7},{-20,-7},{1,1},{123,45}};
  for(int i=0;i<6;i++){ int a=pairs[i][0],b=pairs[i][1];
    if(mix(a,b)!=cmix(a,b)) return 1; }
  return 0;
}`)
	require.Equal(t, 0, code)
}

func TestE2ELong64(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("lmul", ir.ClsL).Export()
	a, b := f.Param("a", ir.ClsL), f.Param("b", ir.ClsL)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsL, e.Mul(ir.ClsL, a, b), f.Long(1<<40)))

	_, code := buildAndRun(t, m, `
#include <stdint.h>
extern long lmul(long,long);
int main(void){
  long r = lmul(1000000LL, 1000000LL);
  return r == 1000000000000LL + (1LL<<40) ? 0 : 1;
}`)
	require.Equal(t, 0, code)
}

func TestE2EMemoryAlloc(t *testing.T) {
	// Allocate 8 bytes, store a computed value, load it back, return it.
	m := ir.NewModule()
	f := m.NewFunc("memtest", ir.ClsL).Export()
	x := f.Param("x", ir.ClsL)
	e := f.Entry()
	p := e.Alloc(8, 8)
	e.Store(e.Mul(ir.ClsL, x, f.Long(3)), p)
	e.Ret(e.Load(ir.ClsL, p))

	_, code := buildAndRun(t, m, `
extern long memtest(long);
int main(void){ return memtest(14)==42 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// TestE2ESpill forces spilling by keeping many values live simultaneously, then
// checks the result is still correct.
func TestE2ESpill(t *testing.T) {
	const n = 40
	m := ir.NewModule()
	f := m.NewFunc("bigsum", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	vals := make([]ir.Ref, n)
	for i := 0; i < n; i++ {
		vals[i] = e.Add(ir.ClsW, x, f.Word(int64(i)))
	}
	acc := f.Word(0)
	for i := 0; i < n; i++ {
		acc = e.Add(ir.ClsW, acc, vals[i])
	}
	e.Ret(acc)

	// sum_{i=0}^{n-1} (x+i) = n*x + n(n-1)/2
	_, code := buildAndRun(t, m, `
extern int bigsum(int);
int main(void){ int x=5; int want=40*x + 40*39/2; return bigsum(x)==want?0:1; }`)
	require.Equal(t, 0, code)
}

func TestE2EZeroExtendSpillWritesFullResult(t *testing.T) {
	const valueCount = 40
	module := ir.NewModule()
	function := module.NewFunc("sumzeroextended", ir.ClsL).Export()
	input := function.Param("input", ir.ClsW)
	entry := function.Entry()
	values := make([]ir.Ref, valueCount)
	for index := range values {
		word := entry.Add(ir.ClsW, input, function.Word(int64(index)))
		values[index] = entry.Extuw(ir.ClsL, word)
	}
	sum := function.Long(0)
	for _, value := range values {
		sum = entry.Add(ir.ClsL, sum, value)
	}
	entry.Ret(sum)

	_, code := buildAndRun(t, module, `
#include <stdint.h>
extern unsigned long sumzeroextended(unsigned int);
__attribute__((noinline)) static void poison_stack(void) {
  volatile uint64_t words[256];
  for (int i = 0; i < 256; i++) words[i] = UINT64_MAX;
}
int main(void) {
  poison_stack();
  return sumzeroextended(1) == 820 ? 0 : 1;
}`)
	require.Equal(t, 0, code)
}

// TestE2EAllocInNonEntryBlock is a regression test for a stack-offset bug the
// QBE corpus surfaced: an alloc in a block other than the entry was assigned the
// wrong frame offset because the offset map was keyed by a global instruction
// position but looked up by a per-block index.
func TestE2EAllocInNonEntryBlock(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("allo", ir.ClsL).Export()
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	b := f.NewBlock("b")
	start.Goto(b)
	p := b.Alloc(8, 8)
	b.Store(b.Extsw(ir.ClsL, n), p)
	b.Ret(b.Load(ir.ClsL, p))

	_, code := buildAndRun(t, m, `
extern long allo(int);
int main(void){ return allo(42)==42 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestE2EStackArgsInt(t *testing.T) {
	// sum10 receives 10 int params (2 on the stack); go calls it with 10 args
	// (2 passed on the stack). Exercises both sides of AAPCS64 stack passing.
	m := ir.NewModule()
	sum := m.NewFunc("sum10", ir.ClsW).Export()
	var ps []ir.Ref
	for i := 0; i < 10; i++ {
		ps = append(ps, sum.Param(string(rune('a'+i)), ir.ClsW))
	}
	e := sum.Entry()
	acc := ps[0]
	for i := 1; i < 10; i++ {
		acc = e.Add(ir.ClsW, acc, ps[i])
	}
	e.Ret(acc)

	g := m.NewFunc("go10", ir.ClsW).Export()
	ge := g.Entry()
	var args []ir.Ref
	for i := 1; i <= 10; i++ {
		args = append(args, g.Word(int64(i)))
	}
	ge.Ret(ge.Call(ir.ClsW, g.Sym("sum10", 0), args...))

	_, code := buildAndRun(t, m, `
extern int sum10(int,int,int,int,int,int,int,int,int,int);
extern int go10(void);
int main(void){
  if (sum10(1,2,3,4,5,6,7,8,9,10) != 55) return 1;
  if (go10() != 55) return 2;   // cg12 calling cg12 with stacked args
  return 0;
}`)
	require.Equal(t, 0, code)
}

func TestE2EStackArgsFloat(t *testing.T) {
	// 10 double params: v0..v7 then two on the stack.
	m := ir.NewModule()
	f := m.NewFunc("dsum10", ir.ClsD).Export()
	var ps []ir.Ref
	for i := 0; i < 10; i++ {
		ps = append(ps, f.Param(string(rune('a'+i)), ir.ClsD))
	}
	e := f.Entry()
	acc := ps[0]
	for i := 1; i < 10; i++ {
		acc = e.Add(ir.ClsD, acc, ps[i])
	}
	e.Ret(acc)

	_, code := buildAndRun(t, m, `
extern double dsum10(double,double,double,double,double,double,double,double,double,double);
int main(void){
  double r = dsum10(0.5,1,1.5,2,2.5,3,3.5,4,4.5,5); // = 27.5
  return r == 27.5 ? 0 : 1;
}`)
	require.Equal(t, 0, code)
}

func TestE2ECallExternal(t *testing.T) {
	// Our function calls a C-provided helper triple(), then adds 1.
	m := ir.NewModule()
	f := m.NewFunc("usehelper", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, e.Call(ir.ClsW, f.Sym("triple", 0), x), f.Word(1)))

	_, code := buildAndRun(t, m, `
int triple(int x){ return x*3; }
extern int usehelper(int);
int main(void){ return usehelper(10)==31 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

func TestE2EUnsignedCompareAndDiv(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("uop", ir.ClsW).Export()
	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
	e := f.Entry()
	lt := e.Cmp(ir.CmpUlt, ir.ClsW, a, b) // unsigned a<b
	q := e.UDiv(ir.ClsW, a, b)            // unsigned division
	e.Ret(e.Add(ir.ClsW, lt, q))

	_, code := buildAndRun(t, m, `
#include <stdint.h>
extern int uop(unsigned,unsigned);
int main(void){
  unsigned a=0xfffffff0u, b=7u;
  unsigned want = (a<b?1u:0u) + a/b;
  return uop(a,b)==(int)want ? 0 : 1;
}`)
	require.Equal(t, 0, code)
}

func TestE2EStackedAggregateArg(t *testing.T) {
	// Eight word arguments fill x0..x7, so a by-value pair passed as the ninth
	// argument spills onto the stack. The C callee reads it there and returns the
	// sum of its fields, exercising the caller-side stacked-aggregate path.
	m := ir.NewModule()
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m.AddType(pair)
	f := m.NewFunc("caller", ir.ClsW).Export()
	e := f.Entry()
	p := e.Alloc(8, 8)
	e.Store(f.Word(30), p)                            // p.a = 30
	e.Store(f.Word(12), e.Add(ir.ClsL, p, f.Long(4))) // p.b = 12
	args := []ir.Ref{f.Word(0), f.Word(0), f.Word(0), f.Word(0), f.Word(0), f.Word(0), f.Word(0), f.Word(0), p}
	r := e.Call(ir.ClsW, f.Sym("sink", 0), args...)
	call := &e.Instrs[len(e.Instrs)-1]
	call.AggArgs = make([]*ir.AggType, 9)
	call.AggArgs[8] = pair
	e.Ret(r)

	_, code := buildAndRun(t, m, `
struct pair { int a, b; };
int sink(int a0,int a1,int a2,int a3,int a4,int a5,int a6,int a7, struct pair p){
  return p.a + p.b;
}
extern int caller(void);
int main(void){ return caller() == 42 ? 0 : 1; }`)
	require.Equal(t, 0, code)
}
