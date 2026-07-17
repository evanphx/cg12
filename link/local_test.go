package link_test

import (
	"runtime"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/link"
	"github.com/stretchr/testify/require"
)

// helperModule builds a module with a private helper returning ret, and an
// exported entry that calls it. Naming both modules' helper the same is the
// point: `static` means each belongs to its own object.
func helperModule(entry string, ret int64) *ir.Module {
	m := ir.NewModule()
	h := m.NewFunc("helper", ir.ClsW) // private: no .Export()
	h.Entry().Ret(h.Word(ret))

	f := m.NewFunc(entry, ir.ClsW).Export()
	f.Entry().Ret(f.Entry().Call(ir.ClsW, f.Sym("helper", 0)))
	return m
}

// A private symbol must not answer another object's external reference. Object B
// calls a `helper` it never defines; that is an unresolved reference, not a
// silent binding to whatever private helper happens to be linked in.
func TestLocalSymbolDoesNotSatisfyAnExternalReference(t *testing.T) {
	a := ir.NewModule()
	h := a.NewFunc("helper", ir.ClsW) // private to a
	h.Entry().Ret(h.Word(111))

	b := ir.NewModule()
	g := b.NewFunc("main", ir.ClsW).Export()
	g.Entry().Ret(g.Entry().Call(ir.ClsW, g.Sym("helper", 0)))

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(a))
	require.NoError(t, l.AddModule(b))
	_, err := l.LinkExecutable("main")
	require.Error(t, err, "b's call to an undefined helper must not bind to a's private one")
}

// Two objects may each have their own private helper of the same name, and each
// must reach its own.
func TestPrivateSymbolsOfTheSameNameStaySeparate(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(helperModule("first", 40)))
	require.NoError(t, l.AddModule(helperModule("second", 2)))

	// main = first() + second(); each must call its own helper: 40 + 2.
	m := ir.NewModule()
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, e.Call(ir.ClsW, f.Sym("first", 0)), e.Call(ir.ClsW, f.Sym("second", 0))))
	require.NoError(t, l.AddModule(m))

	exe, err := l.LinkExecutable("main")
	require.NoError(t, err)
	require.Equal(t, 42, runExe(t, exe), "each unit calls its own helper")
}
