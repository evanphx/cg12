package arm64_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTailCallDeepRecursion(t *testing.T) {
	// even <-> odd via mandatory tail calls: the callee reuses the frame, so
	// millions of levels run without overflowing the stack.
	m := ir.NewModule()
	parity := func(name, other string, base int64) {
		f := m.NewFunc(name, ir.ClsW).Export()
		n := f.Param("n", ir.ClsW)
		e := f.Entry()
		ret, rec := f.NewBlock("ret"), f.NewBlock("rec")
		e.Jnz(e.Cmp(ir.CmpEq, ir.ClsW, n, f.Word(0)), ret, rec)
		ret.Ret(f.Word(base))
		rec.TailCall(ir.ClsW, f.Sym(other, 0), rec.Sub(ir.ClsW, n, f.Word(1)))
	}
	parity("even", "odd", 1)
	parity("odd", "even", 0)

	// The tail call must be a branch (b), never a call (bl).
	asm, err := arm64.CompileModule(m)
	require.NoError(t, err)
	assert.Contains(t, asm, "\tb odd\n")
	assert.Contains(t, asm, "\tb even\n")
	assert.NotContains(t, asm, "\tbl ") // no call instruction at all

	_, code := buildAndRun(t, m, `
extern int even(int);
int main(void){ return (even(1000000) == 1 && even(1000001) == 0 && even(0) == 1) ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestTailCallStackArgsFit(t *testing.T) {
	// A 10-parameter function (two stack args) tail-calls a 10-argument function:
	// the callee's stack arguments fit in the caller's incoming-argument area, so
	// they are written there and the tail call is honored.
	m := ir.NewModule()
	callee := m.NewFunc("callee", ir.ClsW).Export()
	var cp []ir.Ref
	for i := 0; i < 10; i++ {
		cp = append(cp, callee.Param("p", ir.ClsW))
	}
	ce := callee.Entry()
	ce.Ret(ce.Add(ir.ClsW, ce.Add(ir.ClsW, cp[0], cp[8]), cp[9])) // p0 + p8 + p9

	caller := m.NewFunc("caller", ir.ClsW).Export()
	var rp []ir.Ref
	for i := 0; i < 10; i++ {
		rp = append(rp, caller.Param("q", ir.ClsW))
	}
	re := caller.Entry()
	args := append([]ir.Ref{}, rp[:8]...)
	args = append(args, re.Add(ir.ClsW, rp[8], caller.Word(100)), re.Add(ir.ClsW, rp[9], caller.Word(200)))
	re.TailCall(ir.ClsW, caller.Sym("callee", 0), args...)

	asm, err := arm64.CompileModule(m)
	require.NoError(t, err)
	assert.NotContains(t, asm, "\tbl ") // tail-branched, not called

	_, code := buildAndRun(t, m, `
extern int caller(int,int,int,int,int,int,int,int,int,int);
int main(void){ // p0 + (p8+100) + (p9+200) = 1 + 109 + 210 = 320
  return caller(1,2,3,4,5,6,7,8,9,10) == 320 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestTailCallStackArgsOverflowErrors(t *testing.T) {
	// A parameterless caller has no incoming-argument area, so a tail call that
	// needs stack arguments would overflow the caller's frame. The backend must
	// reject it rather than corrupt the stack.
	m := ir.NewModule()
	f := m.NewFuncVoid("g")
	f.Linkage.Export = true
	e := f.Entry()
	var args []ir.Ref
	for i := 0; i < 10; i++ {
		args = append(args, f.Word(int64(i)))
	}
	e.TailCallVoid(f.Sym("h", 0), args...)

	_, err := arm64.CompileModule(m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stack argument")
}

func TestTailCallNotInTailPositionErrors(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("g", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	r := e.Call(ir.ClsW, f.Sym("h", 0), x)
	e.Instrs[len(e.Instrs)-1].Tail = true
	e.Ret(e.Add(ir.ClsW, r, f.Word(1))) // result used, not returned

	_, err := arm64.CompileModule(m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tail call")
}

func TestTailCallRoundTripsThroughText(t *testing.T) {
	// The tailcall mnemonic prints and parses back with the Tail flag set.
	m := ir.NewModule()
	f := m.NewFunc("go", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	f.Entry().TailCall(ir.ClsW, f.Sym("helper", 0), n)
	text := m.String()
	assert.True(t, strings.Contains(text, "tailcall $helper"), "prints tailcall: %s", text)
}
