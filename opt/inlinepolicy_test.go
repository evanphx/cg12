package opt

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// The inlining policy, tested as a policy.
//
// Everything else that checks inlining does so by running the pipeline over a
// module and counting what came out, which answers "did this call disappear" but
// not "which rule decided it". That gap is why finding out why a six-instruction
// function would not inline into sqlite3VdbeExec meant bisecting the constants
// one at a time: two gates were rejecting it, and lifting either alone showed no
// change at all, so the experiment could not distinguish "this is not the rule"
// from "this is one of two rules".
//
// These tests name the rule instead. Each builds the smallest module that
// isolates one gate and asserts the reason inlineDecision gives, so a rule that
// changes has to be changed here too, deliberately. They are in package opt
// rather than opt_test because the decision is internal: what is being pinned is
// the policy, not the observable effect of the whole pipeline on a large input.

// leaf builds a callee named n whose body is `size` adds and a return, so its
// funcSize is predictable. Everything here needs a callee of a chosen size, and
// counting instructions by hand in each test would make the sizes silently drift
// from the budgets they are being tested against.
func leaf(m *ir.Module, n string, size int) *ir.Func {
	f := m.NewFunc(n, ir.ClsW)
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	v := x
	for i := 0; i < size; i++ {
		v = e.Add(ir.ClsW, v, f.Word(int64(i+1)))
	}
	e.Ret(v)
	return f
}

// callerOf builds a function calling callee once, and returns it with the call.
func callerOf(m *ir.Module, name string, callee *ir.Func) (*ir.Func, *ir.Instr) {
	f := m.NewFunc(name, ir.ClsW)
	p := f.Param("p", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Call(ir.ClsW, f.Sym(callee.Name, 0), p))
	for i := range e.Instrs {
		if e.Instrs[i].Op == ir.OCall {
			return f, &e.Instrs[i]
		}
	}
	panic("no call emitted")
}

// decide runs the real decision for the one call in caller.
func decide(t *testing.T, caller *ir.Func, call *ir.Instr, callee *ir.Func, sites int) inlineReason {
	t.Helper()
	return inlineDecision(caller, call, callee, sites, nil, false)
}

// A small single-site callee inlines. This is the baseline every other case is a
// deviation from, so if it ever stops holding the rest of the file is testing
// nothing.
func TestInlinePolicyAcceptsSmallSingleSiteCallee(t *testing.T) {
	m := ir.NewModule()
	callee := leaf(m, "small", 3)
	caller, call := callerOf(m, "caller", callee)
	require.Equal(t, inlineAccept, decide(t, caller, call, callee, 1))
}

// The size budget is scaled by how many call sites the callee has: a body copied
// into many places must be small to be worth the duplication, while a lone site
// merely moves the body. The floor is inlineSmallBudget, so past a handful of
// sites the budget stops shrinking.
func TestInlinePolicySizeBudgetScalesWithCallSites(t *testing.T) {
	for _, tc := range []struct {
		name  string
		size  int
		sites int
		want  inlineReason
	}{
		{"one site, at the budget", inlineOnceBudget, 1, inlineAccept},
		{"one site, over the budget", inlineOnceBudget + 1, 1, reasonOverSizeLimit},
		// The floor: dividing by sites would give 24/8=3, but inlineSmallBudget
		// holds it at 24, so a small callee still inlines at many sites.
		{"many sites, under the floor", inlineSmallBudget, 8, inlineAccept},
		{"many sites, over the floor", inlineSmallBudget + 1, 8, reasonOverSizeLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := ir.NewModule()
			// funcSize counts the return block's instructions too, so build the body
			// one short and confirm the size landed where the case intends.
			callee := leaf(m, "c", tc.size)
			require.Equal(t, tc.size, funcSize(callee), "test builds a callee of the size it claims")
			caller, call := callerOf(m, "caller", callee)
			require.Equal(t, tc.want, decide(t, caller, call, callee, tc.sites))
		})
	}
}

// noinline beats everything, including forceinline. The two attributes are
// mutually exclusive, but if both are set the safe resolution is not to inline.
func TestInlinePolicyNoInlineWinsOverForceInline(t *testing.T) {
	m := ir.NewModule()
	callee := leaf(m, "cold", 3)
	callee.NoInline = true
	callee.ForceInline = true
	caller, call := callerOf(m, "caller", callee)
	require.Equal(t, reasonNoInlineAttr, decide(t, caller, call, callee, 1))
}

// forceinline and the cost model's choice both bypass the size budget -- that is
// the whole point of them -- but neither bypasses the structural rules.
func TestInlinePolicyForcedCalleesBypassSizeButNotStructure(t *testing.T) {
	t.Run("size is bypassed", func(t *testing.T) {
		for _, set := range []func(*ir.Func){
			func(f *ir.Func) { f.ForceInline = true },
			func(f *ir.Func) { f.CostInline = true },
		} {
			m := ir.NewModule()
			callee := leaf(m, "big", inlineOnceBudget*4)
			set(callee)
			caller, call := callerOf(m, "caller", callee)
			require.Equal(t, inlineAccept, decide(t, caller, call, callee, 1))
		}
	})
	t.Run("structure is not", func(t *testing.T) {
		m := ir.NewModule()
		callee := leaf(m, "va", 2)
		callee.Variadic = true
		callee.ForceInline = true
		caller, call := callerOf(m, "caller", callee)
		require.Equal(t, reasonVariadic, decide(t, caller, call, callee, 1))
	})
}

// The structural refusals, each isolated. These are correctness rules rather
// than cost ones: splicing any of these bodies would produce wrong code, not
// merely bigger code, so they are the ones that must never be relaxed for speed.
func TestInlinePolicyStructuralRefusals(t *testing.T) {
	t.Run("no body", func(t *testing.T) {
		m := ir.NewModule()
		callee := m.NewFunc("external", ir.ClsW)
		callee.Param("x", ir.ClsW)
		caller, call := callerOf(m, "caller", callee)
		require.Equal(t, reasonNoBody, decide(t, caller, call, callee, 1))
	})
	t.Run("variadic", func(t *testing.T) {
		m := ir.NewModule()
		callee := leaf(m, "va", 2)
		callee.Variadic = true
		caller, call := callerOf(m, "caller", callee)
		require.Equal(t, reasonVariadic, decide(t, caller, call, callee, 1))
	})
	t.Run("no return to splice onto", func(t *testing.T) {
		m := ir.NewModule()
		callee := m.NewFunc("noreturn", ir.ClsW)
		callee.Param("x", ir.ClsW)
		callee.Entry().Hlt()
		caller, call := callerOf(m, "caller", callee)
		require.Equal(t, reasonNoReturn, decide(t, caller, call, callee, 1))
	})
	t.Run("computed goto in the body", func(t *testing.T) {
		m := ir.NewModule()
		callee := m.NewFunc("dispatch", ir.ClsW)
		x := callee.Param("x", ir.ClsW)
		e := callee.Entry()
		target := callee.NewBlock("t")
		target.Ret(x)
		e.Jmp = ir.Jmp{Kind: ir.JmpBr, Arg: x, Targets: []*ir.Block{target}}
		caller, call := callerOf(m, "caller", callee)
		require.Equal(t, reasonComputedGoto, decide(t, caller, call, callee, 1))
	})
}

// Arity is checked at the call site, not the callee: the same callee is fine
// from a matching call and refused from a mismatched one. A mismatch reaches the
// inliner through a prototype-less C declaration, and splicing it would bind
// parameters to arguments that are not there.
func TestInlinePolicyArityIsACallSiteProperty(t *testing.T) {
	m := ir.NewModule()
	callee := leaf(m, "one_param", 3)

	good, goodCall := callerOf(m, "good", callee)
	require.Equal(t, inlineAccept, decide(t, good, goodCall, callee, 1))

	bad := m.NewFunc("bad", ir.ClsW)
	p := bad.Param("p", ir.ClsW)
	e := bad.Entry()
	e.Ret(e.Call(ir.ClsW, bad.Sym("one_param", 0), p, p)) // two arguments, one parameter
	var badCall *ir.Instr
	for i := range e.Instrs {
		if e.Instrs[i].Op == ir.OCall {
			badCall = &e.Instrs[i]
		}
	}
	require.Equal(t, reasonArity, decide(t, bad, badCall, callee, 1))
}

// An indirect call has no callee to reason about. The decision must say so
// rather than crash, since findInlinable hands it whatever directCallee resolved.
func TestInlinePolicyIndirectCallHasNoCallee(t *testing.T) {
	m := ir.NewModule()
	caller := m.NewFunc("caller", ir.ClsW)
	fp := caller.Param("fp", ir.ClsL)
	e := caller.Entry()
	e.Ret(e.Call(ir.ClsW, fp))
	var call *ir.Instr
	for i := range e.Instrs {
		if e.Instrs[i].Op == ir.OCall {
			call = &e.Instrs[i]
		}
	}
	require.Equal(t, reasonIndirect, decide(t, caller, call, nil, 0))
}

// The caller-level gate is the one that cost the most to diagnose, because it
// rejects every call in a function at once: from any single call site, a caller
// over budget is indistinguishable from a callee that is too big. It is asserted
// here as a property of the caller alone.
func TestInlinePolicyCallerSizeGateIsIndependentOfTheCallee(t *testing.T) {
	m := ir.NewModule()
	callee := leaf(m, "tiny", 2)

	small, smallCall := callerOf(m, "small_caller", callee)
	require.False(t, inlineCallerOverBudget(small))
	require.Equal(t, inlineAccept, decide(t, small, smallCall, callee, 1),
		"the callee is inlinable on its own terms")

	// The same callee, from a caller past the size ceiling. The per-call decision
	// still says yes -- the gate lives in inlineInto, one level up -- which is
	// exactly the split that made this hard to see.
	big := m.NewFunc("big_caller", ir.ClsW)
	p := big.Param("p", ir.ClsW)
	e := big.Entry()
	v := p
	for i := 0; i < inlineCallerInstrBudget+16; i++ {
		v = e.Add(ir.ClsW, v, big.Word(1))
	}
	e.Ret(e.Call(ir.ClsW, big.Sym("tiny", 0), v))
	require.True(t, inlineCallerOverBudget(big),
		"a caller this large is over the ceiling, so nothing inlines into it")
}

// The diagnostic must not change what it reports on. It nearly did: routing the
// callee lookup through directCallee recorded a dependency into the inliner's
// memo, and the emitted object then differed depending on whether the
// environment variable was set -- a measuring instrument altering the
// measurement.
func TestInlinePolicyDiagnosticDoesNotRecordDependencies(t *testing.T) {
	m := ir.NewModule()
	callee := leaf(m, "helper", 3)
	caller, call := callerOf(m, "caller", callee)
	cg := buildCallGraph(m)

	require.Nil(t, activeDeps, "this test assumes no recorder is installed")
	got := lookupCallee(caller, call, cg.byName)
	require.Equal(t, callee, got, "the diagnostic still has to resolve the callee")

	// And it tolerates what it will be handed: an indirect call has no symbol.
	indirect := m.NewFunc("indirect", ir.ClsW)
	fp := indirect.Param("fp", ir.ClsL)
	e := indirect.Entry()
	e.Ret(e.Call(ir.ClsW, fp))
	for i := range e.Instrs {
		if e.Instrs[i].Op == ir.OCall {
			require.Nil(t, lookupCallee(indirect, &e.Instrs[i], cg.byName))
		}
	}
}

// A caller past the size ceiling still takes a tiny leaf. This is the one
// exemption to the caller gate, and both of its conditions matter: without the
// size limit or without the leaf requirement it becomes the change that was
// measured and rejected -- SQLite 3.7% faster, Lua 4.7% slower with its
// interpreter's spills going 71 to 130.
func TestInlinePolicyOverBudgetCallerStillTakesTinyLeaves(t *testing.T) {
	overBudget := func(m *ir.Module, calleeName string) *ir.Func {
		f := m.NewFunc("huge", ir.ClsW)
		p := f.Param("p", ir.ClsW)
		e := f.Entry()
		v := p
		for i := 0; i < inlineCallerInstrBudget+16; i++ {
			v = e.Add(ir.ClsW, v, f.Word(1))
		}
		e.Ret(e.Call(ir.ClsW, f.Sym(calleeName, 0), v))
		return f
	}

	t.Run("a tiny leaf is inlined", func(t *testing.T) {
		m := ir.NewModule()
		leaf(m, "tiny", inlineTinyLeafSize)
		caller := overBudget(m, "tiny")
		require.True(t, inlineCallerOverBudget(caller), "the premise: this caller is over the ceiling")
		require.True(t, Inline(m))
		require.Zero(t, callsIn(caller), "the tiny leaf should have been folded in")
	})

	t.Run("one instruction over the size limit is not", func(t *testing.T) {
		m := ir.NewModule()
		leaf(m, "justbig", inlineTinyLeafSize+1)
		caller := overBudget(m, "justbig")
		require.False(t, Inline(m))
		require.Equal(t, 1, callsIn(caller))
	})

	t.Run("a tiny NON-leaf is not", func(t *testing.T) {
		// Small enough on its own, but it calls out of the module, so it can never
		// become a leaf. That distinction is real: a callee whose own callee is
		// inlinable gets flattened first and then qualifies on its own merits, which
		// is why this uses a call the inliner can never remove rather than one it
		// merely has not removed yet.
		m := ir.NewModule()
		mid := m.NewFunc("mid", ir.ClsW)
		x := mid.Param("x", ir.ClsW)
		me := mid.Entry()
		me.Ret(me.Call(ir.ClsW, mid.Sym("external_not_in_module", 0), x))
		require.LessOrEqual(t, funcSize(mid), inlineTinyLeafSize, "the premise: mid is tiny")

		caller := overBudget(m, "mid")
		Inline(m)
		require.Equal(t, 1, callsIn(caller), "a non-leaf must not be exempt from the ceiling")
	})
}

// allocsIn counts the stack slots a function reserves.
func allocsIn(f *ir.Func) int {
	n := 0
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			if b.Instrs[i].Op.IsAlloc() {
				n++
			}
		}
	}
	return n
}

// callsIn counts the calls left in a function.
func callsIn(f *ir.Func) int {
	n := 0
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			if b.Instrs[i].Op == ir.OCall {
				n++
			}
		}
	}
	return n
}

// The reasons must be distinct and non-empty, because they are the output of a
// diagnostic: two gates sharing a string would send a reader to the wrong rule,
// which is the failure mode this whole file exists to prevent.
func TestInlinePolicyReasonsAreDistinct(t *testing.T) {
	all := []inlineReason{
		reasonIndirect, reasonFrameScoped, reasonClosureCtx, reasonArity,
		reasonRecursive, reasonNoBody, reasonSecondEntry, reasonVariadic,
		reasonAggParam, reasonAggReturn, reasonComputedGoto, reasonFrameInBody,
		reasonCallerFrame, reasonNoReturn, reasonNoInlineAttr, reasonOverSizeLimit,
		reasonOtherPass, reasonCallerTooLarge, reasonCallerGrown, reasonPassBudget,
	}
	seen := map[inlineReason]bool{}
	for _, r := range all {
		require.NotEqual(t, inlineAccept, r, "a refusal must not be the accept value")
		require.False(t, seen[r], "duplicate reason text: %q", r)
		require.False(t, strings.HasSuffix(string(r), "."), "reasons read as fragments, not sentences: %q", r)
		seen[r] = true
	}
}

// A by-value aggregate parameter no longer refuses a callee, and the gates that
// remain still compound.
//
// QuickJS's JS_CallInternal is the hot function of cc/testdata/perf/quickjs_bench.c,
// and cg12 compiles that workload to code 5.3x slower than clang's -- against
// 1.31x on SQLite and 1.24x on Lua. Asked with a census that reports every rule
// refusing a site rather than only the first, the by-value aggregate parameter
// was the single largest thing in the way and the only one of the three programs
// it touched at all: it was the sole refusal on 16,050 QuickJS call sites,
// against 7 on SQLite and none on Lua. JSValue is 16 bytes and passed by value
// throughout, so JS_DupValue (437 sites), set_value (294) and JS_IsException
// (283) all wore it.
//
// It was never a cost judgement. inlinableStructure's comment said the splice
// "does not model" such a parameter, and spliceCall now does: the parameter gets
// its own storage and the argument is copied into it, which is what an actual
// call would give it.
//
// The lesson that outlived it is that the remaining gates still compound, so
// lifting one and measuring no change does not mean it was not a gate. Lifting
// the recursion-cycle refusal on its own moved none of the three workloads,
// because most of what sat behind it was refused for being variadic or over
// budget as well.
func TestInlinePolicyAggregateParameterNoLongerRefuses(t *testing.T) {
	// aggLeaf is JS_IsException's shape: tiny, a leaf, and taking its argument as
	// a by-value aggregate.
	aggLeaf := func(m *ir.Module, n string) *ir.Func {
		f := m.NewFunc(n, ir.ClsW)
		p := f.Param("v", ir.ClsL)
		f.Temp(p).Agg = &ir.AggType{Name: "JSValue", Size: 16, Align: 8, Opaque: true}
		e := f.Entry()
		e.Ret(e.Load(ir.ClsW, p))
		return f
	}

	t.Run("it inlines", func(t *testing.T) {
		m := ir.NewModule()
		callee := aggLeaf(m, "JS_IsException")
		caller, call := callerOf(m, "interp", callee)
		require.Equal(t, inlineAccept, decide(t, caller, call, callee, 283),
			"an aggregate parameter is copied in, not refused")
		require.Empty(t, inlineRefusals(caller, call, callee, 283, nil, false))
	})

	// The copy is made where it can be observed and skipped where it cannot. A
	// callee that only reads its parameter reads the caller's object directly and
	// costs nothing; copying those regardless cost QuickJS 17.6% of its text and
	// pushed eleven of the Go runtime's nosplit chains past their frame budget.
	t.Run("a read-only parameter is not copied", func(t *testing.T) {
		m := ir.NewModule()
		callee := aggLeaf(m, "reader")
		caller, _ := callerOf(m, "interp", callee)
		require.True(t, Inline(m))
		require.Zero(t, callsIn(caller), "the callee should have been spliced in")
		require.Zero(t, allocsIn(caller), "reading needs no copy of its own")
	})

	t.Run("a written parameter gets storage of its own", func(t *testing.T) {
		m := ir.NewModule()
		callee := m.NewFunc("writer", ir.ClsW)
		pv := callee.Param("v", ir.ClsL)
		callee.Temp(pv).Agg = &ir.AggType{Name: "JSValue", Size: 16, Align: 8, Opaque: true}
		e := callee.Entry()
		e.Store(callee.Word(7), pv) // assigns to its own parameter
		e.Ret(e.Load(ir.ClsW, pv))

		caller, _ := callerOf(m, "interp", callee)
		require.True(t, Inline(m))
		require.Zero(t, callsIn(caller), "the callee should have been spliced in")
		require.NotZero(t, allocsIn(caller),
			"the callee writes to its parameter, so it must not write to the caller's object")
	})

	// And the gates that remain still compound: two refusals look like one to
	// anything that reports only the first.
	t.Run("remaining gates still compound", func(t *testing.T) {
		m := ir.NewModule()
		callee := aggLeaf(m, "vararg")
		callee.Variadic = true
		caller, call := callerOf(m, "interp", callee)
		require.Equal(t, reasonVariadic, decide(t, caller, call, callee, 283))
	})
}

// inlineRefusals must agree with the decision it explains.
//
// It is a deliberate second copy of the same predicates, written not to
// short-circuit so a reader can see past the first refusal. A second copy drifts
// unless something holds it: the rule is that the FIRST refusal it lists is
// exactly what inlineDecision returns, and that it lists nothing when the call is
// accepted. Every case in this file is run through both.
func TestInlineRefusalsAgreeWithTheDecision(t *testing.T) {
	check := func(t *testing.T, caller *ir.Func, call *ir.Instr, callee *ir.Func, sites int, accept func(*ir.Func) bool) {
		t.Helper()
		want := inlineDecision(caller, call, callee, sites, accept, false)
		got := inlineRefusals(caller, call, callee, sites, accept, false)
		if want == inlineAccept {
			require.Empty(t, got, "the call is accepted, so nothing refuses it")
			return
		}
		require.NotEmpty(t, got, "the call is refused, so something must refuse it")
		require.Equal(t, want, got[0], "the first refusal is the decision")
	}

	t.Run("accepted", func(t *testing.T) {
		m := ir.NewModule()
		callee := leaf(m, "small", 2)
		caller, call := callerOf(m, "c", callee)
		check(t, caller, call, callee, 1, nil)
	})

	t.Run("indirect", func(t *testing.T) {
		m := ir.NewModule()
		callee := leaf(m, "small", 2)
		caller, call := callerOf(m, "c", callee)
		check(t, caller, call, nil, 1, nil)
	})

	t.Run("over the size limit", func(t *testing.T) {
		m := ir.NewModule()
		callee := leaf(m, "big", inlineSmallBudget+40)
		caller, call := callerOf(m, "c", callee)
		check(t, caller, call, callee, 1, nil)
	})

	t.Run("noinline", func(t *testing.T) {
		m := ir.NewModule()
		callee := leaf(m, "no", 2)
		callee.NoInline = true
		caller, call := callerOf(m, "c", callee)
		check(t, caller, call, callee, 1, nil)
	})

	t.Run("variadic", func(t *testing.T) {
		m := ir.NewModule()
		callee := leaf(m, "va", 2)
		callee.Variadic = true
		caller, call := callerOf(m, "c", callee)
		check(t, caller, call, callee, 1, nil)
	})

	t.Run("refused by the pass filter", func(t *testing.T) {
		m := ir.NewModule()
		callee := leaf(m, "other", 2)
		caller, call := callerOf(m, "c", callee)
		check(t, caller, call, callee, 1, func(*ir.Func) bool { return false })
	})

	// And the case the whole thing exists for: several rules at once. The first is
	// still the decision, and the rest are what a single-reason census hid.
	t.Run("several rules at once", func(t *testing.T) {
		m := ir.NewModule()
		callee := leaf(m, "manyfaults", inlineSmallBudget+40)
		callee.Variadic = true
		callee.NoInline = true
		caller, call := callerOf(m, "c", callee)
		check(t, caller, call, callee, 1, nil)
		require.Greater(t, len(inlineRefusals(caller, call, callee, 1, nil, false)), 1,
			"three rules refuse this call and the census must be able to say so")
	})
}
