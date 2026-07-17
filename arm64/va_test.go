package arm64

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
)

func TestCompileVariadic(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("v", ir.ClsL).Export()
	f.Variadic = true
	f.Param("x", ir.ClsL) // one named param -> namedGr = 1
	e := f.Entry()
	ap := e.Alloc(8, 32)
	e.VaStart(ap)
	i := e.VaArg(ir.ClsL, ap)
	d := e.VaArg(ir.ClsD, ap)
	_ = d
	e.Ret(i)

	asm := disasmModule(t, m)
	// Register save area written at entry.
	assert.Contains(t, asm, "str x1", "argument registers spilled to the save area")
	assert.Contains(t, asm, "str d0")
	// vaarg emits a branch on the register offset.
	assert.Contains(t, asm, "b.ge")
	assert.Contains(t, asm, "sxtw", "save-area address computed with sign-extended offset")
}

func TestComputeNamedCounts(t *testing.T) {
	m := ir.NewModule()
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m.AddType(pair)
	vec := &ir.AggType{Name: "vec2", Fields: []ir.Field{{Sub: ir.SubS}, {Sub: ir.SubS}}}
	m.AddType(vec)
	big := &ir.AggType{Name: "big", Fields: []ir.Field{{Sub: ir.SubB, Count: 24}}}
	m.AddType(big)

	f := m.NewFuncVoid("f")
	f.Param("a", ir.ClsW) // x0
	f.Param("b", ir.ClsD) // v0
	// aggregate named params of each class
	for name, agg := range map[string]*ir.AggType{"p": pair, "v": vec, "m": big} {
		r := f.NewTemp(name, ir.ClsL)
		f.Temp(r).Agg = agg
		f.Params = append(f.Params, f.Temp(r))
	}
	ngrn, nsrn, _ := computeNamedCounts(f)
	// a (x0) + big pointer (x1) + pair (x2) = 3 GP; b (v0) + vec (v1,v2) = 3 SIMD.
	assert.Equal(t, 3, ngrn)
	assert.Equal(t, 3, nsrn)
}

func TestVariadicPrints(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("f", ir.ClsW)
	f.Variadic = true
	f.Param("x", ir.ClsL)
	f.Entry().Ret(f.Word(0))
	assert.True(t, strings.Contains(f.String(), "$f(l %x, ...)"))
}
