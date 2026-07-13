package amd64_test

import (
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// The cooperative poll strategy: a function with an OSafepoint loads a flag and,
// when set, calls a runtime stub. Built and run end-to-end under qemu with the
// flag, stub, and observable global all in the same module.
func pollModule(setFlag bool) *ir.Module {
	m := ir.NewModule()
	m.Data = append(m.Data,
		&ir.Data{Name: "g_poll", Align: 4, Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{0}}}},
		&ir.Data{Name: "g_seen", Align: 4, Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{0}}}},
	)

	// poll_stub records that it ran, then returns.
	stub := m.NewFuncVoid("poll_stub")
	se := stub.Entry()
	se.Store(stub.Word(1), stub.Sym("g_seen", 0))
	se.RetVoid()

	// work has an explicit safepoint (where the poll is emitted).
	work := m.NewFuncVoid("work")
	we := work.Entry()
	we.Safepoint()
	we.RetVoid()

	f := m.NewFunc("runtest", ir.ClsW).Export()
	e := f.Entry()
	if setFlag {
		e.Store(f.Word(1), f.Sym("g_poll", 0))
	}
	e.CallVoid(f.Sym("work", 0))
	// Success (0) when: flag set -> stub ran (g_seen==1); flag clear -> stub did
	// not run (g_seen==0).
	seen := e.Load(ir.ClsW, f.Sym("g_seen", 0))
	if setFlag {
		e.Ret(e.Sub(ir.ClsW, seen, f.Word(1)))
	} else {
		e.Ret(seen)
	}
	return m
}

func TestObjGCPollFlagClear(t *testing.T) {
	opts := amd64.Options{GC: amd64.PollStrategy{FlagSym: "g_poll", StubSym: "poll_stub"}}
	require.Equal(t, 0, runObjWith(t, pollModule(false), opts))
}

func TestObjGCPollFlagSet(t *testing.T) {
	opts := amd64.Options{GC: amd64.PollStrategy{FlagSym: "g_poll", StubSym: "poll_stub"}}
	require.Equal(t, 0, runObjWith(t, pollModule(true), opts))
}

// The stack-growth guard's fast path: with the limit set very low, rsp-frame is
// always above it, so the guard passes and the function proceeds normally. Proves
// the prologue guard compiles and executes (the grow path needs a real runtime).
func TestObjStackGrowthFastPath(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data,
		&ir.Data{Name: "g_limit", Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{0}}}},
	)
	// A never-called morestack, present so the relocation resolves.
	ms := m.NewFuncVoid("cg12_morestack")
	ms.Entry().RetVoid()

	// A guarded function that does real work.
	add := m.NewFunc("add", ir.ClsW)
	a := add.Param("a", ir.ClsW)
	b := add.Param("b", ir.ClsW)
	add.Entry().Ret(add.Entry().Add(ir.ClsW, a, b))

	f := m.NewFunc("runtest", ir.ClsW).Export()
	e := f.Entry()
	e.Ret(e.Sub(ir.ClsW, e.Call(ir.ClsW, f.Sym("add", 0), f.Word(20), f.Word(22)), f.Word(42)))

	opts := amd64.Options{GC: amd64.StackGrowth{LimitSym: "g_limit", MorestackSym: "cg12_morestack"}}
	require.Equal(t, 0, runObjWith(t, m, opts))
}
