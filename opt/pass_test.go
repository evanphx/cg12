package opt_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/assert"
)

func TestFuncPassRunsOverAllFunctions(t *testing.T) {
	m := ir.NewModule()
	for _, name := range []string{"a", "b", "c"} {
		f := m.NewFunc(name, ir.ClsW)
		f.Entry().Ret(f.Word(0))
	}
	var seen []string
	p := opt.FuncPass("collect", func(f *ir.Func) bool {
		seen = append(seen, f.Name)
		return false
	})
	assert.Equal(t, "collect", p.Name())
	assert.False(t, p.Run(m)) // no function reported a change
	assert.Equal(t, []string{"a", "b", "c"}, seen)
}

func TestModulePassChangePropagates(t *testing.T) {
	m := ir.NewModule()
	calls := 0
	p := opt.ModulePass("count", func(*ir.Module) bool { calls++; return true })
	assert.True(t, p.Run(m))
	assert.Equal(t, 1, calls)
}

func TestFixpointIteratesUntilStable(t *testing.T) {
	// A pass that reports change three times, then stabilises: the fixpoint must
	// run it until it returns false, and itself report that something changed.
	m := ir.NewModule()
	remaining := 3
	rounds := 0
	step := opt.ModulePass("step", func(*ir.Module) bool {
		rounds++
		if remaining > 0 {
			remaining--
			return true
		}
		return false
	})
	fp := opt.Fixpoint("loop", step)
	assert.True(t, fp.Run(m))
	assert.Equal(t, 0, remaining)
	assert.Equal(t, 4, rounds) // 3 changing rounds + 1 that reports stable
}

func TestFixpointStableReportsNoChange(t *testing.T) {
	m := ir.NewModule()
	fp := opt.Fixpoint("loop", opt.ModulePass("noop", func(*ir.Module) bool { return false }))
	assert.False(t, fp.Run(m))
}

func TestDefaultPipelineShape(t *testing.T) {
	// The pipeline exists and is non-trivial; a smoke check that Run applies it.
	m := ir.NewModule()
	f := m.NewFunc("id", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	f.Entry().Ret(x)
	assert.NotEmpty(t, opt.DefaultPipeline())
	opt.Run(m, opt.DefaultPipeline()) // must not panic on a trivial module
	assert.True(t, hasFunc(m, "id"))
}

func TestBoundedPipelineShape(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("main", ir.ClsW)
	entry := f.Entry()
	value := entry.Copy(ir.ClsW, f.Word(1))
	entry.Ret(value)

	assert.NotEmpty(t, opt.BoundedPipeline())
	opt.Run(m, opt.BoundedPipeline())
	assert.Equal(t, f.Word(1), entry.Jmp.Arg)
}
