package wasm_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/wasm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parityModule builds mutually-recursive even/odd where the recursive call is a
// mandatory tail call, so deep recursion runs in constant stack.
func parityModule() *ir.Module {
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
	return m
}

func TestE2ETailCallDeepRecursion(t *testing.T) {
	m := parityModule()
	src, err := wasm.CompileModule(m)
	require.NoError(t, err)
	assert.Contains(t, src, "return_call $odd")
	assert.Contains(t, src, "return_call $even")

	// 5,000,000 levels — impossible without tail-call elimination.
	require.Equal(t, "1", runFunc(t, m, "even", "5000000"))
	require.Equal(t, "0", runFunc(t, m, "even", "5000001"))
	require.Equal(t, "1", runFunc(t, m, "even", "0"))
}

func TestTailCallManyArgsAllowedOnWasm(t *testing.T) {
	// wasm has no register/stack-argument limit, so a 9-argument tail call — which
	// arm64 must reject — compiles here. This is the platform difference the tail
	// marker lets the author encode.
	m := ir.NewModule()
	// callee sums its 9 arguments.
	sum := m.NewFunc("sum9", ir.ClsW).Export()
	var ps []ir.Ref
	for i := 0; i < 9; i++ {
		ps = append(ps, sum.Param("a", ir.ClsW))
	}
	se := sum.Entry()
	acc := sum.Word(0)
	for _, p := range ps {
		acc = se.Add(ir.ClsW, acc, p)
	}
	se.Ret(acc)
	// caller tail-calls it with nine constants.
	f := m.NewFunc("go", ir.ClsW).Export()
	e := f.Entry()
	var args []ir.Ref
	for i := 1; i <= 9; i++ {
		args = append(args, f.Word(int64(i)))
	}
	e.TailCall(ir.ClsW, f.Sym("sum9", 0), args...)

	src, err := wasm.CompileModule(m)
	require.NoError(t, err)
	assert.Contains(t, src, "return_call $sum9")
	require.Equal(t, "45", runFunc(t, m, "go")) // 1+2+...+9
}

func TestTailCallNotInTailPositionErrors(t *testing.T) {
	// A tail-marked call whose result is used (not returned) must be rejected.
	m := ir.NewModule()
	f := m.NewFunc("g", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	r := e.Call(ir.ClsW, f.Sym("h", 0), x)
	e.Instrs[len(e.Instrs)-1].Tail = true
	e.Ret(e.Add(ir.ClsW, r, f.Word(1))) // result used, not tail

	_, err := wasm.CompileModule(m)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tail call")
}
