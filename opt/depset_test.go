package opt

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func depNames(funcs []*ir.Func) []string {
	out := make([]string, 0, len(funcs))
	for _, f := range funcs {
		out = append(out, f.Name)
	}
	return out
}

// depChain builds leaf <- mid <- top, each small enough to inline, plus `wide`,
// a callee far over the size budget that top calls and the inliner therefore
// reads but never splices. It is the smallest module that separates the two
// recorded sets.
func depChain(m *ir.Module) (top, mid, leaf, wide *ir.Func) {
	leaf = m.NewFunc("leaf", ir.ClsW)
	leafArg := leaf.Param("x", ir.ClsW)
	leaf.Entry().Ret(leaf.Entry().Add(ir.ClsW, leafArg, leaf.Word(1)))

	mid = m.NewFunc("mid", ir.ClsW)
	midArg := mid.Param("x", ir.ClsW)
	midEntry := mid.Entry()
	midEntry.Ret(midEntry.Add(ir.ClsW, midEntry.Call(ir.ClsW, mid.Sym("leaf", 0), midArg), mid.Word(2)))

	wide = m.NewFunc("wide", ir.ClsW)
	wideArg := wide.Param("x", ir.ClsW)
	wideEntry := wide.Entry()
	value := wideArg
	for step := 0; step < 4*inlineSmallBudget; step++ {
		value = wideEntry.Add(ir.ClsW, value, wide.Word(int64(step+1)))
	}
	wideEntry.Ret(value)

	top = m.NewFunc("top", ir.ClsW).Export()
	topArg := top.Param("x", ir.ClsW)
	topEntry := top.Entry()
	topEntry.Ret(topEntry.Add(ir.ClsW,
		topEntry.Call(ir.ClsW, top.Sym("mid", 0), topArg),
		topEntry.Call(ir.ClsW, top.Sym("wide", 0), topArg)))
	return top, mid, leaf, wide
}

func moduleText(m *ir.Module) string {
	var builder strings.Builder
	for _, f := range m.Funcs {
		builder.WriteString(f.String())
		builder.WriteString("\n")
	}
	return builder.String()
}

// The spliced set has to be transitively closed, because a caller that received
// a clone of mid's body received the copy of leaf that was already inside it. A
// key that named only mid would not notice leaf changing.
func TestRecordClosesTheSplicedSetThroughTheCalleesOwnInlines(t *testing.T) {
	module := ir.NewModule()
	top, _, _, _ := depChain(module)

	deps := Record(func() { OptimizeModule(module) })

	assert.Equal(t, []string{"leaf", "mid"}, depNames(deps.Spliced(top)),
		"top holds a clone of mid, which already held a clone of leaf")
}

// The consulted set is the one soundness needs, and it is strictly larger: the
// inliner read `wide` -- its size, its structure, its attributes -- and decided
// against it. Had `wide` been smaller the decision would have gone the other
// way, so a key that omits it is a key that can hand back a stale body.
func TestRecordKeepsACalleeItReadButDidNotInline(t *testing.T) {
	module := ir.NewModule()
	top, _, _, _ := depChain(module)

	deps := Record(func() { OptimizeModule(module) })

	assert.Contains(t, depNames(deps.Consulted(top)), "wide")
	assert.NotContains(t, depNames(deps.Spliced(top)), "wide")
	require.True(t, hasCallTo(top, "wide"), "the fixture only works while wide stays a call")
	for _, spliced := range deps.Spliced(top) {
		assert.Contains(t, depNames(deps.Consulted(top)), spliced.Name,
			"consulted must be a superset of spliced")
	}
}

func hasCallTo(caller *ir.Func, name string) bool {
	for _, block := range caller.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.Op != ir.OCall {
				continue
			}
			callee := instruction.Arg(0)
			if callee.Kind == ir.RefConst && caller.Consts[callee.ID].Kind == ir.ConstSym &&
				caller.Consts[callee.ID].Sym == name {
				return true
			}
		}
	}
	return false
}

// The whole measurement is worthless if recording perturbs what is measured.
func TestRecordingDoesNotChangeWhatTheOptimiserProduces(t *testing.T) {
	plain := ir.NewModule()
	depChain(plain)
	OptimizeModule(plain)

	recorded := ir.NewModule()
	depChain(recorded)
	Record(func() { OptimizeModule(recorded) })

	require.Equal(t, moduleText(plain), moduleText(recorded))
}

func TestRecordRestoresTheRecorderAndRefusesToNest(t *testing.T) {
	require.Nil(t, activeDeps, "a recorder outlived an earlier test")
	Record(func() {
		require.NotNil(t, activeDeps)
		assert.Panics(t, func() { Record(func() {}) })
	})
	require.Nil(t, activeDeps, "Record must leave the compiler unrecorded")
}

// callSiteCounts is a whole-module quantity feeding a per-function decision,
// which would be the one dependency a per-function key could not express. It
// cannot change a decision today, because the two budget constants are equal:
// inlineOnceBudget/sites is floored at inlineSmallBudget, and the floor is the
// same number as the numerator. This test is what lets the report say so, and it
// fails the moment someone separates the constants and re-arms the dependency.
func TestCallSiteCountCannotChangeAnInlineDecisionToday(t *testing.T) {
	require.Equal(t, inlineOnceBudget, inlineSmallBudget,
		"the site count is a live input again; the per-function key now needs it")

	module := ir.NewModule()
	atBudget := module.NewFunc("atBudget", ir.ClsW)
	arg := atBudget.Param("x", ir.ClsW)
	entry := atBudget.Entry()
	value := arg
	for step := 0; step < inlineSmallBudget; step++ {
		value = entry.Add(ir.ClsW, value, atBudget.Word(int64(step+1)))
	}
	entry.Ret(value)
	require.Equal(t, inlineSmallBudget, funcSize(atBudget))

	for _, sites := range []int{1, 2, 7, 115, 10000} {
		assert.True(t, worthInlining(atBudget, sites),
			"a callee at exactly the budget inlines regardless of its %d call sites", sites)
	}
}
