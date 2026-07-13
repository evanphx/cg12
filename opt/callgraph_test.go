package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// leafFn defines name(x) = x + 1 with no calls.
func leafFn(m *ir.Module, name string) *ir.Func {
	f := m.NewFunc(name, ir.ClsW)
	x := f.Param("x", ir.ClsW)
	f.Entry().Ret(f.Entry().Add(ir.ClsW, x, f.Word(1)))
	return f
}

// callerFn defines name(x) = <sum of calling each callee with x>.
func callerFn(m *ir.Module, name string, callees ...string) *ir.Func {
	f := m.NewFunc(name, ir.ClsW)
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	acc := f.Word(0)
	for _, c := range callees {
		acc = e.Add(ir.ClsW, acc, e.Call(ir.ClsW, f.Sym(c, 0), x))
	}
	e.Ret(acc)
	return f
}

func TestCallGraphSCC(t *testing.T) {
	// even <-> odd (mutual recursion); self (direct recursion); leaf (none);
	// top calls leaf and even (in no cycle itself).
	m := ir.NewModule()
	callerFn(m, "even", "odd")
	callerFn(m, "odd", "even")
	callerFn(m, "self", "self")
	leafFn(m, "leaf")
	callerFn(m, "top", "leaf", "even")

	cg := buildCallGraph(m)
	scc := computeSCC(cg)
	byName := map[string]*ir.Func{}
	for _, f := range m.Funcs {
		byName[f.Name] = f
	}

	// Mutual recursion shares a component; direct recursion is flagged too.
	assert.Equal(t, scc.comp[byName["even"]], scc.comp[byName["odd"]])
	assert.NotEqual(t, scc.comp[byName["even"]], scc.comp[byName["leaf"]])
	assert.True(t, scc.recursive[byName["even"]])
	assert.True(t, scc.recursive[byName["odd"]])
	assert.True(t, scc.recursive[byName["self"]])
	assert.False(t, scc.recursive[byName["leaf"]])
	assert.False(t, scc.recursive[byName["top"]])

	// order is bottom-up: for every cross-SCC edge f -> g, g precedes f.
	pos := map[*ir.Func]int{}
	for i, f := range scc.order {
		pos[f] = i
	}
	require.Len(t, scc.order, len(m.Funcs))
	for _, f := range m.Funcs {
		for _, g := range cg.callees[f] {
			if scc.comp[f] != scc.comp[g] {
				assert.Less(t, pos[g], pos[f], "callee %s before caller %s", g.Name, f.Name)
			}
		}
	}
}
