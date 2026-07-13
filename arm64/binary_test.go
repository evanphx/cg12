package arm64_test

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBinaryUnitCompilesAndRuns caches a module to bytes, reloads it, and
// compiles the reloaded unit — proving a cached unit is a complete, usable
// module (the point of the format: skip the front end on a cache hit).
func TestBinaryUnitCompilesAndRuns(t *testing.T) {
	build := func() *ir.Module {
		m := ir.NewModule()
		f := m.NewFunc("f", ir.ClsW).Export()
		a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
		e := f.Entry()
		// ((a + b) * a) - (b / 2)
		s := e.Add(ir.ClsW, a, b)
		p := e.Mul(ir.ClsW, s, a)
		e.Ret(e.Sub(ir.ClsW, p, e.Div(ir.ClsW, b, f.Word(2))))
		return m
	}

	// Compilation mutates the module, so encode the pristine one first.
	data, err := build().MarshalBinary()
	require.NoError(t, err)

	decoded, err := ir.DecodeModule(data)
	require.NoError(t, err)

	// Two fresh decodes compile to identical assembly (deterministic + complete).
	m1, _ := ir.DecodeModule(data)
	m2, _ := ir.DecodeModule(data)
	asm1, err := arm64.CompileModule(m1)
	require.NoError(t, err)
	asm2, err := arm64.CompileModule(m2)
	require.NoError(t, err)
	assert.Equal(t, asm1, asm2)

	// The decoded unit runs correctly: f(3,4) = (7*3) - (4/2) = 19.
	_, code := buildAndRun(t, decoded, `
extern int f(int, int);
int main(void){ return f(3, 4) == 19 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}
