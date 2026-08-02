package opt

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The summary analysis is easiest to state as IR, because that is what it reads.
// Each of these builds one small module by hand and checks one property of the
// answer: the dereference depth, the leak target, the fixed point, or the
// directive.

// storeParamIntoGlobal is `func f(p *T) { global = p }`: the pointer itself is
// published, so the parameter escapes at depth 0.
func TestEscapeFactsParameterStoredIntoGlobal(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("f")
	parameter := function.Param("p", ir.ClsP)
	entry := function.Entry()
	entry.Store(parameter, function.Sym("global", 0))
	entry.RetVoid()

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamEscapes, facts.Param("f", 0).Escape)
}

// A parameter that is only read through does not escape: the load is one
// dereference above it, so the heap never reaches the parameter at depth zero.
func TestEscapeFactsParameterOnlyRead(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("f", ir.ClsL)
	parameter := function.Param("p", ir.ClsP)
	entry := function.Entry()
	entry.Ret(entry.Load(ir.ClsL, parameter))

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamNoEscape, facts.Param("f", 0).Escape)
}

// The distinction a boolean cannot express: `global = *p` publishes what p
// points at, not p. The pointee escapes; the pointer does not, so a caller
// passing the address of a frame object may keep that object in the frame.
func TestEscapeFactsPointeeEscapesButPointerDoesNot(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("f")
	parameter := function.Param("p", ir.ClsP)
	entry := function.Entry()
	entry.Store(entry.Load(ir.ClsP, parameter), function.Sym("global", 0))
	entry.RetVoid()

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamNoEscape, facts.Param("f", 0).Escape,
		"storing *p publishes the pointee, and says nothing about whether p's own storage has to outlive the call")
}

// Returning the parameter is a leak to the result, not an escape: the caller
// decides, by what it does with the result.
func TestEscapeFactsParameterReturned(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("f", ir.ClsP)
	parameter := function.Param("p", ir.ClsP)
	entry := function.Entry()
	entry.Ret(parameter)

	facts := ComputeEscapeFacts(module)

	fact := facts.Param("f", 0)
	assert.Equal(t, ParamLeaksToResult, fact.Escape)
	assert.Equal(t, 0, fact.Result)
}

// A frontend variable is a frame slot written and read back. The summary has to
// see through that, or every parameter of every function goc emits would look
// like it escapes at its first store.
func TestEscapeFactsThroughAFrameSlot(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("f")
	parameter := function.Param("p", ir.ClsP)
	entry := function.Entry()
	slot := entry.Alloc(8, 8)
	entry.Store(parameter, slot)
	entry.Store(entry.Load(ir.ClsP, slot), function.Sym("global", 0))
	entry.RetVoid()

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamEscapes, facts.Param("f", 0).Escape,
		"the parameter reaches the global through its own variable slot")
}

// A frame slot written and read back that goes nowhere keeps the parameter
// local. This is the control for the previous test: without it, that one would
// pass for a summary that escaped everything it stored.
func TestEscapeFactsThroughAFrameSlotThatGoesNowhere(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("f", ir.ClsL)
	parameter := function.Param("p", ir.ClsP)
	entry := function.Entry()
	slot := entry.Alloc(8, 8)
	entry.Store(parameter, slot)
	entry.Ret(entry.Load(ir.ClsL, entry.Load(ir.ClsP, slot)))

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamNoEscape, facts.Param("f", 0).Escape)
}

// A callee's summary is what the caller's own summary is computed from: `f`
// passes its parameter to `g`, which publishes it, so `f`'s parameter escapes
// too. This is the whole point of a bottom-up solve.
func TestEscapeFactsPropagateThroughACall(t *testing.T) {
	module := ir.NewModule()
	leaky := module.NewFuncVoid("g")
	leakyParam := leaky.Param("p", ir.ClsP)
	leakyEntry := leaky.Entry()
	leakyEntry.Store(leakyParam, leaky.Sym("global", 0))
	leakyEntry.RetVoid()

	caller := module.NewFuncVoid("f")
	callerParam := caller.Param("q", ir.ClsP)
	callerEntry := caller.Entry()
	callerEntry.CallVoid(caller.Sym("g", 0), callerParam)
	callerEntry.RetVoid()

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamEscapes, facts.Param("g", 0).Escape)
	assert.Equal(t, ParamEscapes, facts.Param("f", 0).Escape)
}

// The same shape with a callee that retains nothing: the caller's parameter must
// come out clean. Without summaries the pass assumes the worst at the call and
// this is the case it gets wrong.
func TestEscapeFactsCleanCalleeKeepsCallerClean(t *testing.T) {
	module := ir.NewModule()
	callee := module.NewFunc("g", ir.ClsL)
	calleeParam := callee.Param("p", ir.ClsP)
	calleeEntry := callee.Entry()
	calleeEntry.Ret(calleeEntry.Load(ir.ClsL, calleeParam))

	caller := module.NewFunc("f", ir.ClsL)
	callerParam := caller.Param("q", ir.ClsP)
	callerEntry := caller.Entry()
	callerEntry.Ret(callerEntry.Call(ir.ClsL, caller.Sym("g", 0), callerParam))

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamNoEscape, facts.Param("g", 0).Escape)
	assert.Equal(t, ParamNoEscape, facts.Param("f", 0).Escape)
}

// Mutual recursion. goc's walk answers "escapes" the moment a query re-enters a
// function already on its stack, so this pair is pessimised there whatever the
// bodies do. The fixed point converges instead.
func TestEscapeFactsMutualRecursionConverges(t *testing.T) {
	module := ir.NewModule()
	first := module.NewFuncVoid("f")
	firstParam := first.Param("p", ir.ClsP)
	second := module.NewFuncVoid("g")
	secondParam := second.Param("p", ir.ClsP)

	firstEntry := first.Entry()
	firstEntry.CallVoid(first.Sym("g", 0), firstParam)
	firstEntry.RetVoid()
	secondEntry := second.Entry()
	secondEntry.CallVoid(second.Sym("f", 0), secondParam)
	secondEntry.RetVoid()

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamNoEscape, facts.Param("f", 0).Escape)
	assert.Equal(t, ParamNoEscape, facts.Param("g", 0).Escape)
	assert.Equal(t, 1, facts.Stats.NonTrivialSCCs)
	assert.Equal(t, 2, facts.Stats.FuncsInNonTrivialSCCs)

	broken := ComputeEscapeFactsBreakingCycles(module)
	assert.Equal(t, ParamEscapes, broken.Param("f", 0).Escape,
		"the cycle-breaking rule is what goc's walk does, and it is what this is measured against")
}

// A cycle that does publish must still come out escaping: the optimistic start
// has to be driven down by the direct publication, or the fixed point is
// unsound rather than merely more precise.
func TestEscapeFactsRecursionThatPublishesStillEscapes(t *testing.T) {
	module := ir.NewModule()
	first := module.NewFuncVoid("f")
	firstParam := first.Param("p", ir.ClsP)
	second := module.NewFuncVoid("g")
	secondParam := second.Param("p", ir.ClsP)

	firstEntry := first.Entry()
	firstEntry.CallVoid(first.Sym("g", 0), firstParam)
	firstEntry.RetVoid()
	secondEntry := second.Entry()
	secondEntry.Store(secondParam, second.Sym("global", 0))
	secondEntry.CallVoid(second.Sym("f", 0), secondParam)
	secondEntry.RetVoid()

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamEscapes, facts.Param("g", 0).Escape)
	assert.Equal(t, ParamEscapes, facts.Param("f", 0).Escape)
}

// Self recursion is a cycle too, and the same rule applies to it.
func TestEscapeFactsSelfRecursion(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("f")
	parameter := function.Param("p", ir.ClsP)
	entry := function.Entry()
	entry.CallVoid(function.Sym("f", 0), parameter)
	entry.RetVoid()

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamNoEscape, facts.Param("f", 0).Escape)
	assert.Equal(t, 1, facts.Stats.SelfRecursiveFuncs)
}

// A bodiless callee is assumed to retain everything, unless the front end put
// //go:noescape on it -- which is the only escape fact such a function has.
func TestEscapeFactsNoEscapeDirective(t *testing.T) {
	for _, declared := range []bool{false, true} {
		module := ir.NewModule()
		module.NewFuncVoid("external") // no body

		caller := module.NewFuncVoid("f")
		callerParam := caller.Param("p", ir.ClsP)
		callerEntry := caller.Entry()
		callerEntry.CallVoid(caller.Sym("external", 0), callerParam)
		callerEntry.RetVoid()

		if declared {
			module.SymAttrs = map[string]ir.SymAttr{"external": ir.SymNoEscape}
		}

		facts := ComputeEscapeFacts(module)

		want := ParamEscapes
		if declared {
			want = ParamNoEscape
		}
		assert.Equal(t, want, facts.Param("f", 0).Escape, "//go:noescape declared: %v", declared)
	}
}

// An indirect call cannot be summarised, so it has to keep assuming the worst.
func TestEscapeFactsIndirectCallEscapes(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("f")
	parameter := function.Param("p", ir.ClsP)
	target := function.Param("fn", ir.ClsP)
	entry := function.Entry()
	entry.CallVoid(target, parameter)
	entry.RetVoid()

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamEscapes, facts.Param("f", 0).Escape)
}

// The counts the report quotes come from here, so they have to be counts of
// something checked rather than of whatever the solver happened to do.
func TestEscapeFactsStatsCountComponents(t *testing.T) {
	module := ir.NewModule()
	for _, name := range []string{"a", "b", "c"} {
		function := module.NewFuncVoid(name)
		function.Param("p", ir.ClsP)
		function.Entry().RetVoid()
	}

	facts := ComputeEscapeFacts(module)

	require.Equal(t, 3, facts.Stats.Funcs)
	assert.Equal(t, 3, facts.Stats.Bodied)
	assert.Equal(t, 3, facts.Stats.Components)
	assert.Equal(t, 0, facts.Stats.NonTrivialSCCs)
	assert.Equal(t, 3, facts.Stats.Params)
	assert.Equal(t, 3, facts.Stats.NoEscape)
}

// A frame allocation is one location, not one per word. goc builds a closure
// environment as a single alloc and stores the captures at 8, 16 and 24, so an
// analysis that escaped only the word the published pointer named would miss
// every capture -- which is exactly how runtime.newproc came out retaining
// nothing.
func TestEscapeFactsPublishingAnAllocationEscapesAllOfIt(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFuncVoid("f")
	parameter := function.Param("p", ir.ClsP)
	entry := function.Entry()
	region := entry.Alloc(8, 32)
	entry.Store(parameter, entry.Add(ir.ClsP, region, function.Long(8)))
	entry.Store(region, function.Sym("global", 0))
	entry.RetVoid()

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamEscapes, facts.Param("f", 0).Escape,
		"the published pointer names offset 0 and the parameter is at offset 8, but one allocation escapes as a whole")
}

// A closure environment has to be treated as escaping outright. Its captures are
// read back through the body's %closure register, in a different ir.Func, and no
// dataflow over one function can follow that -- so runtime.newproc, which stores
// fn into the environment of the literal it hands to the //go:noescape
// systemstack, would otherwise be summarised as retaining nothing.
func TestEscapeFactsClosureEnvironmentEscapes(t *testing.T) {
	module := ir.NewModule()
	body := module.NewFuncVoid("f.literal")
	body.Entry().RetVoid()
	module.NewFuncVoid("consume") // stands in for a //go:noescape callee

	function := module.NewFuncVoid("f")
	parameter := function.Param("p", ir.ClsP)
	entry := function.Entry()
	environment := entry.Alloc(8, 16)
	entry.Store(function.Sym("f.literal", 0), environment)
	entry.Store(parameter, entry.Add(ir.ClsP, environment, function.Long(8)))
	entry.CallVoid(function.Sym("consume", 0), environment)
	entry.RetVoid()
	module.SymAttrs = map[string]ir.SymAttr{"consume": ir.SymNoEscape}

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamEscapes, facts.Param("f", 0).Escape,
		"a value captured by a closure may be retained by whatever the closure is handed to")
}

// The control for the previous test: an ordinary data symbol stored into a
// frame allocation is not a closure, and must not escape everything around it.
func TestEscapeFactsDataSymbolIsNotAClosure(t *testing.T) {
	module := ir.NewModule()
	module.NewFuncVoid("consume")

	function := module.NewFuncVoid("f")
	parameter := function.Param("p", ir.ClsP)
	entry := function.Entry()
	storage := entry.Alloc(8, 16)
	entry.Store(function.Sym("some.data", 0), storage)
	entry.Store(parameter, entry.Add(ir.ClsP, storage, function.Long(8)))
	entry.CallVoid(function.Sym("consume", 0), storage)
	entry.RetVoid()
	module.SymAttrs = map[string]ir.SymAttr{"consume": ir.SymNoEscape}

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamNoEscape, facts.Param("f", 0).Escape)
}

// A pointer handed to a callee inside an aggregate the caller owns is retained
// by that callee, and the summary has to say so.
//
// This is CCWORK_REPORT.md section 2.2's reduction as IR, and it is the shape
// goc's calling convention produces for any struct wider than a couple of
// words: the argument is the address of a frame slot, and what the callee keeps
// is a pointer it loads out of that slot. publish's own summary is correct and
// nearly useless -- it does not retain the address it was handed -- and reading
// that as "publish retains nothing" is what let a package-level variable end up
// holding a dead frame's address three calls away.
func TestEscapeFactsPointerPassedInsideAFrameAggregateEscapes(t *testing.T) {
	module := ir.NewModule()

	publish := module.NewFuncVoid("publish")
	request := publish.Param("r", ir.ClsP)
	publishing := publish.Entry()
	publishing.Store(publishing.Load(ir.ClsP, request), publish.Sym("sink", 0))
	publishing.RetVoid()

	hold := module.NewFuncVoid("hold")
	pointer := hold.Param("x", ir.ClsP)
	holding := hold.Entry()
	aggregate := holding.Alloc(8, 8)
	holding.Store(pointer, aggregate)
	holding.CallVoid(hold.Sym("publish", 0), aggregate)
	holding.RetVoid()

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamNoEscape, facts.Param("publish", 0).Escape,
		"publish keeps the pointer it loads out of r, not r itself, which is what a depth of one means")
	assert.Equal(t, ParamEscapes, facts.Param("hold", 0).Escape,
		"x is written into an aggregate publish loads through, so publish retains x past the call")
}

// The precision control for the previous test. Forwarding an ordinary pointer
// parameter to a callee that does not retain it must still summarise as
// noescape: publishing the argument's pointee at depth one is a statement about
// what the pointer points at, and a parameter reached at depth one is exactly
// the answer ParamNoEscape stands for.
//
// Without this, the fix to the semantics would collapse the whole table to
// "everything escapes" and the summaries would be worth nothing.
func TestEscapeFactsForwardedPointerStillDoesNotEscape(t *testing.T) {
	module := ir.NewModule()

	inner := module.NewFunc("inner", ir.ClsL)
	innerParameter := inner.Param("p", ir.ClsP)
	innerEntry := inner.Entry()
	innerEntry.Ret(innerEntry.Load(ir.ClsL, innerParameter))

	outer := module.NewFunc("outer", ir.ClsL)
	outerParameter := outer.Param("p", ir.ClsP)
	outerEntry := outer.Entry()
	outerEntry.Ret(outerEntry.Call(ir.ClsL, outer.Sym("inner", 0), outerParameter))

	facts := ComputeEscapeFacts(module)

	assert.Equal(t, ParamNoEscape, facts.Param("inner", 0).Escape)
	assert.Equal(t, ParamNoEscape, facts.Param("outer", 0).Escape,
		"forwarding a pointer to a callee that only reads through it retains nothing")
}
