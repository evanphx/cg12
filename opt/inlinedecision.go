package opt

import (
	"fmt"
	"os"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// The inlining decision, in one place and in order.
//
// A call has to clear eight independent gates before it is inlined, and they
// grew up separately: three bound the caller (its size, its growth allowance,
// how many inlines one pass may do), four describe the callee (recursion,
// structure, size, attributes), and one belongs to the call site. Spread across
// findInlinable, worthInlining, inlinableStructure, inlinableWithBudget and
// inlineInto, no single place said what the policy was, and two of them
// overlapped -- worthInlining re-checked the structure its caller had already
// established.
//
// The practical cost of that was not tidiness. Working out why a six-instruction
// function would not inline into sqlite3VdbeExec took bisecting the constants one
// at a time, because two separate gates were rejecting it and lifting either one
// alone changed nothing. inlineDecision exists so that question is answered by
// asking rather than by experiment, and so the policy can be tested directly
// instead of inferred from what a large program happens to produce.
//
// This changes no decision. It is the same predicates in the same order, with a
// name attached to each outcome.

// inlineReason names the gate that decided a call site. The empty reason means
// the call is inlinable.
type inlineReason string

const (
	inlineAccept inlineReason = ""

	// Call-site properties: nothing about the callee could change these.
	reasonIndirect    inlineReason = "callee is not a direct call to a module function"
	reasonFrameScoped inlineReason = "call reads the physical caller's frame"
	reasonClosureCtx  inlineReason = "closure callee reached without a context operand"
	reasonArity       inlineReason = "call arity does not match the callee's parameters"

	// Callee properties.
	reasonRecursive     inlineReason = "callee is in a recursion cycle"
	reasonNoBody        inlineReason = "callee has no body"
	reasonSecondEntry   inlineReason = "callee has a secondary entry"
	reasonVariadic      inlineReason = "callee is variadic"
	reasonAggParam      inlineReason = "callee takes a by-value aggregate parameter"
	reasonAggReturn     inlineReason = "aggregate-returning inlining is disabled (CG12_NO_AGGINLINE)"
	reasonComputedGoto  inlineReason = "callee's control flow depends on block addresses"
	reasonFrameInBody   inlineReason = "callee body contains a frame-scoped runtime call"
	reasonCallerFrame   inlineReason = "callee reads its own caller's frame (getcallerpc/getcallersp)"
	reasonNoReturn      inlineReason = "callee has no return to splice a continuation onto"
	reasonNoInlineAttr  inlineReason = "callee is marked noinline"
	reasonOverSizeLimit inlineReason = "callee is larger than its size budget"

	// Which pass is asking. The pipeline runs the inliner in two arms -- one for
	// cost-model-forced callees, one budgeted -- and each declines the other's work.
	reasonOtherPass inlineReason = "this inlining pass does not handle this callee"

	// Caller properties. These reject every call in the function at once, which is
	// what made them hard to see from a single call site's behaviour.
	reasonCallerTooLarge inlineReason = "caller is already over inlineCallerInstrBudget"
	reasonCallerGrown    inlineReason = "caller has spent its growth allowance"
	reasonPassBudget     inlineReason = "caller reached inlineFuncBudget for this pass"
)

// inlineDecision reports whether the call at a site may be inlined, and if not
// which gate refused. sites is how many call sites the callee has module-wide,
// which scales its size budget; accept is the current pass's filter.
//
// The order matches what findInlinable and worthInlining did between them, so
// the first reason reported is the one that would have rejected the call before.
// That matters for a diagnostic: a call failing four gates should name the one
// nearest the front, since that is the one to address.
func inlineDecision(caller *ir.Func, call *ir.Instr, callee *ir.Func, sites int, accept func(*ir.Func) bool, noSplitChain bool) inlineReason {
	if callee == nil {
		return reasonIndirect
	}
	if isFrameScopedRuntimeCall(caller, call) {
		return reasonFrameScoped
	}
	if callee.HasClosureContext && !(call.ClosureCall && !call.ClosureContext().IsNone()) {
		return reasonClosureCtx
	}
	if accept != nil && !accept(callee) {
		return reasonOtherPass
	}
	if len(call.Args)-1 != len(callee.Params) {
		return reasonArity
	}
	// A nosplit chain's frame is a byte budget this pass cannot see, so the
	// capability that grows frames is declined anywhere such a chain reaches.
	return calleeInlineReason(callee, sites, !noSplitChain)
}

// calleeInlineReason is the part of the decision that depends only on the callee:
// its attributes, its structure, and its size against the budget for the number
// of sites it is called from.
func calleeInlineReason(callee *ir.Func, sites int, aggParams bool) inlineReason {
	// noinline is honoured before everything, so a cold slow-path helper stays a
	// call and out of the hot inlined body. It also wins over ForceInline: the two
	// are mutually exclusive, and "do not inline" is the safe resolution.
	if callee.NoInline {
		return reasonNoInlineAttr
	}
	if r := inlineStructureReason(callee, aggParams); r != inlineAccept {
		return r
	}
	if callee.ForceInline || callee.CostInline {
		return inlineAccept // mandatory, or the cost model's choice: size bypassed
	}
	if sites < 1 {
		sites = 1
	}
	budget := inlineOnceBudget / sites
	if budget < inlineSmallBudget {
		budget = inlineSmallBudget
	}
	if funcSize(callee) > budget {
		return reasonOverSizeLimit
	}
	return inlineAccept
}

// inlineStructureReason is inlinableStructure with its answer named.
//
// aggParams says whether the asking pass can splice a by-value aggregate
// parameter. Only [spliceCall] under the general inliner can: it gives the
// parameter storage of its own and copies the argument in, which is what an
// actual call would do and what the callee's writes to its own parameter require.
// The other callers -- the pre-escape scans, the unroller,
// InlineIntoNoSplitCallers -- have not been measured against it and pass false,
// which is exactly what they did before the copy-in existed.
func inlineStructureReason(callee *ir.Func, aggParams bool) inlineReason {
	if callee.Start == nil {
		return reasonNoBody
	}
	if hasSecondaryEntry(callee) {
		return reasonSecondEntry
	}
	if callee.Variadic {
		return reasonVariadic
	}
	if callee.RetAgg != nil && os.Getenv("CG12_NO_AGGINLINE") != "" {
		return reasonAggReturn
	}
	if !aggParams {
		for _, p := range callee.Params {
			if p.Agg != nil {
				return reasonAggParam
			}
		}
	}
	hasRet := false
	for _, b := range callee.Blocks {
		// A computed goto or a taken label address makes the body's control flow
		// depend on block addresses; splicing it where those addresses move is
		// unsound, so it stays a call -- as gcc does for address-of-label.
		if b.Jmp.Kind == ir.JmpBr {
			return reasonComputedGoto
		}
		for i := range b.Instrs {
			if b.Instrs[i].Op == ir.OBlockAddr {
				return reasonComputedGoto
			}
			// Distinct from the call-site check of the same name: this is the callee
			// containing such a call, which splicing would reparent onto the wrong
			// frame. Dropping this while consolidating the policy is what
			// TestInlineSkipsFrameScopedDefers and TestInlineSkipsCallerFrameIntrinsics
			// exist to catch, and did.
			if isFrameScopedRuntimeCall(callee, &b.Instrs[i]) {
				return reasonFrameInBody
			}
			if isCallerFrameIntrinsic(&b.Instrs[i]) {
				return reasonCallerFrame
			}
		}
		if b.Jmp.Kind == ir.JmpRet {
			hasRet = true
		}
	}
	if !hasRet {
		return reasonNoReturn
	}
	return inlineAccept
}

// inlineRefusals lists EVERY rule that refuses this call site, in the order
// [inlineDecision] would consult them, and is empty when the site is accepted.
//
// The diagnostic reported only the first refusal, which is the one number that
// cannot answer the question anyone asks of it. Two gates rejecting a call look
// exactly like one gate rejecting it, so "lift this rule and 23,758 sites open
// up" reads off the census and is false: lifting the SCC gate on its own moved
// nothing on any of the three workloads, because the sites behind it were
// mostly refused for being variadic, or over budget, or both, as well. Lua's
// luaO_pushfstring is three instructions with 270 call sites and was filed under
// "recursion cycle"; it is also variadic and was never inlinable at all.
//
// What a reader actually wants is the sites blocked by ONE rule -- those are the
// ones lifting it would free -- and that needs the whole set per site.
//
// It deliberately re-runs the checks rather than sharing code with
// [inlineDecision], because a non-short-circuiting copy is the only way to see
// past the first answer. [TestInlineRefusalsAgreeWithTheDecision] pins the copy
// to the original: the first refusal listed here must be exactly what
// inlineDecision returns, so the two cannot drift.
func inlineRefusals(caller *ir.Func, call *ir.Instr, callee *ir.Func, sites int, accept func(*ir.Func) bool, noSplitChain bool) []inlineReason {
	var out []inlineReason
	add := func(r inlineReason) { out = append(out, r) }

	if callee == nil {
		return []inlineReason{reasonIndirect} // nothing else is knowable
	}
	if isFrameScopedRuntimeCall(caller, call) {
		add(reasonFrameScoped)
	}
	if callee.HasClosureContext && !(call.ClosureCall && !call.ClosureContext().IsNone()) {
		add(reasonClosureCtx)
	}
	if accept != nil && !accept(callee) {
		add(reasonOtherPass)
	}
	if len(call.Args)-1 != len(callee.Params) {
		add(reasonArity)
	}
	if callee.NoInline {
		add(reasonNoInlineAttr)
	}
	if r := inlineStructureReason(callee, !noSplitChain); r != inlineAccept {
		add(r) // structure is itself a chain; its first answer is enough
	}
	// Size is bypassed for a mandatory or cost-model-selected callee, so it is not
	// a refusal for one however big it is.
	if !callee.NoInline && !callee.ForceInline && !callee.CostInline {
		if sites < 1 {
			sites = 1
		}
		budget := inlineOnceBudget / sites
		if budget < inlineSmallBudget {
			budget = inlineSmallBudget
		}
		if funcSize(callee) > budget {
			add(reasonOverSizeLimit)
		}
	}
	return out
}

// lookupCallee resolves a call's target without recording the consultation.
//
// directCallee is the inliner's choke point for reading another function and
// notes every lookup into the dependency memo, which is right for a decision the
// compilation depends on and wrong for a diagnostic: reporting on a compilation
// must not change it. Measured -- routing this through directCallee made the
// emitted object differ depending on whether CG12_INLINE_WHY was set.
func lookupCallee(caller *ir.Func, call *ir.Instr, byName map[string]*ir.Func) *ir.Func {
	c := call.Arg(0)
	if c.Kind != ir.RefConst || int(c.ID) >= len(caller.Consts) || caller.Consts[c.ID].Kind != ir.ConstSym {
		return nil
	}
	return byName[caller.Consts[c.ID].Sym]
}

// reportInlineDecisions prints every call site in caller and what was decided,
// when CG12_INLINE_WHY names the caller (or is "*" for all of them).
//
// This is the tool whose absence cost the most: a call rejected by two gates at
// once looks identical, from the outside, to a call rejected by one, so raising
// a single constant to test a theory reports "no change" either way. It prints
// the caller-level verdict first, because those reject every call in the function
// and are therefore the answer whenever they apply.
func reportInlineDecisions(caller *ir.Func, cg *callGraph, scc *sccInfo, sites map[*ir.Func]int, accept func(*ir.Func) bool) {
	want := os.Getenv("CG12_INLINE_WHY")
	if want == "" || (want != "*" && want != caller.Name) {
		return
	}
	// The caller-level gates first, and all of them, because each rejects every
	// call in the function at once. Reporting only the first would reproduce the
	// original problem: raising one constant to test a theory then reports "no
	// change", because a second gate was refusing the same calls. Two of these were
	// refusing sqlite3VdbeExec simultaneously.
	size := funcSize(caller)
	if inlineCallerOverBudget(caller) {
		fmt.Fprintf(os.Stderr, "inline %s: CALLER GATE: %s (%d instrs > %d)\n",
			caller.Name, reasonCallerTooLarge, functionInstructionCount(caller), inlineCallerInstrBudget)
	}
	if cap := inlineGrowthCap(size); size > cap {
		fmt.Fprintf(os.Stderr, "inline %s: CALLER GATE: %s (%d instrs > cap %d)\n",
			caller.Name, reasonCallerGrown, size, cap)
	}
	fmt.Fprintf(os.Stderr, "inline %s: size=%d growthCap=%d perPassLimit=%d (%s once %d spent)\n",
		caller.Name, size, inlineGrowthCap(size), inlineFuncBudget, reasonPassBudget, inlineFuncBudget)
	for _, b := range caller.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op != ir.OCall {
				continue
			}
			callee := lookupCallee(caller, in, cg.byName)
			name := "<indirect>"
			size := 0
			if callee != nil {
				name = callee.Name
				size = funcSize(callee)
			}
			refusals := inlineRefusals(caller, in, callee, sites[callee], accept, cg.noSplitReach[caller])
			if callee != nil && scc.recursive[callee] {
				refusals = append([]inlineReason{reasonRecursive}, refusals...)
			}
			// Every rule that refuses the site, not just the first: one refusal here
			// means lifting that rule would free this call, and several means it
			// would not. The census downstream counts both.
			verdict := "INLINE"
			if len(refusals) > 0 {
				parts := make([]string, len(refusals))
				for i, r := range refusals {
					parts[i] = string(r)
				}
				verdict = strings.Join(parts, " + ")
			}
			fmt.Fprintf(os.Stderr, "inline %s -> %-40s size=%-5d sites=%-4d %s\n",
				caller.Name, name, size, sites[callee], verdict)
		}
	}
}
