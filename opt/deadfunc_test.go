package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
)

// funcNames returns the names of the module's functions, for order-independent
// membership checks.
func funcNames(m *ir.Module) map[string]bool {
	names := map[string]bool{}
	for _, f := range m.Funcs {
		names[f.Name] = true
	}
	return names
}

func TestDeadFuncElimDropsUnreferenced(t *testing.T) {
	m := ir.NewModule()
	m.NewFunc("helper", ir.ClsL).Entry().RetVoid() // unexported, no references
	main := m.NewFunc("main", ir.ClsW).Export()
	main.Entry().Ret(main.Param("a", ir.ClsW))

	assert.True(t, DeadFuncElim(m))
	assert.False(t, funcNames(m)["helper"], "unreferenced helper removed")
	assert.True(t, funcNames(m)["main"])
}

func TestDeadFuncElimKeepsAddressTakenViaPhi(t *testing.T) {
	// A function referenced only by address through a phi operand (as mem2reg
	// produces from a stored function pointer) must not be dropped, or the
	// address relocation would dangle at link time.
	m := ir.NewModule()
	m.NewFunc("helper", ir.ClsL).Entry().RetVoid()

	caller := m.NewFunc("caller", ir.ClsL).Export()
	start := caller.Entry()
	a := caller.NewBlock("a")
	b := caller.NewBlock("b")
	join := caller.NewBlock("join")
	start.Jnz(caller.Param("c", ir.ClsW), a, b)
	a.Goto(join)
	b.Goto(join)
	// One edge carries the helper's address; the other a plain pointer.
	r := join.Phi(ir.ClsL,
		ir.PhiEdge{From: a, Val: caller.Sym("helper", 0)},
		ir.PhiEdge{From: b, Val: caller.Param("p", ir.ClsL)},
	)
	join.Ret(r)

	DeadFuncElim(m)
	assert.True(t, funcNames(m)["helper"], "helper kept: its address is taken in a phi")
}

func TestDeadFuncElimMatchesSemanticAndLinkerSymbolSpellings(t *testing.T) {
	m := ir.NewModule()
	helper := m.NewFuncVoid("runtime.interfaceDispatchFailure")
	helper.Entry().RetVoid()

	caller := m.NewFuncVoid("caller").Export()
	caller.Entry().CallVoid(caller.Sym("runtime_interfaceDispatchFailure", 0))
	caller.Entry().RetVoid()

	DeadFuncElim(m)
	assert.True(t, funcNames(m)[helper.Name], "linker-spelled reference keeps semantic function name")
}
