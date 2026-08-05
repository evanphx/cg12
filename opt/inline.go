package opt

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// Cost-model constants for CostInline selection, grounded in GCC's inliner rather than
// guessed. maxCostInlineSize is GCC's max-inline-insns-single (70): the largest static
// helper the model folds into a hot caller. costInlineGrowthPct is the per-caller growth cap
// (GCC's inline-unit-growth is 40% for the whole unit; large-function-growth is 100%),
// bounding the icache footprint. It can be generous because the register allocator absorbs
// the inlined bodies without extra spilling -- measured directly: vm_exec_core spills stay
// flat (22->21) while force-inlining five helpers, peak pressure rises 29->40 with no new
// spills. So code size (icache), not register pressure, is the only limit here.
const (
	maxCostInlineSize   = 70
	costInlineGrowthPct = 30
)

// A callee's size budget depends on how many call sites name it. A function with a
// single site inlines up to inlineOnceBudget: doing so moves its body rather than
// duplicating it (the original becomes dead and is dropped), so a large one still
// pays. Each additional site duplicates the body, so the budget falls off as
// inlineOnceBudget/sites, never below inlineSmallBudget -- tiny functions inline
// anywhere, since the call overhead alone rivals the body. This is gcc's inlining
// cost model in miniature (its inline-functions-called-once plus a size/growth
// trade), and it is what lets an interpreter's fast-path helpers fold into the
// dispatch loop without the hundreds of shared helpers cascading in and bloating it.
const (
	inlineSmallBudget = 24 // a multi-site callee inlines only when at most this size
	inlineOnceBudget  = 24 // a single-site callee inlines up to this size
)

// preEscapeAggregateInstrBudget admits slightly larger aggregate-returning
// constructors. Scalar pointer constructors stay on the ordinary budget until
// escape analysis can model every pointer-return path they use.
const preEscapeAggregateInstrBudget = 32

// preEscapeInstrBudget permits a larger aggregate helper to be exposed only
// when it directly consumes a heap-allocation candidate. This is intentionally
// separate from ordinary inlining: its purpose is to preserve caller-visible
// escape information, not to inline every helper of this size.
const preEscapeInstrBudget = 256

// inlineFuncBudget bounds how many calls are inlined into one function per pass,
// a backstop against code-size blow-up; the pipeline's fixpoint applies more on
// later rounds if they remain worthwhile.
const inlineFuncBudget = 64

// inlineGrowthCap bounds how large inlining may make one function, as a multiple of
// its pre-inlining size plus a fixed allowance -- a backstop against a pathological
// cascade of single-site inlines (each individually "free" at the unit level) turning
// one function into the whole program.
//
// A proportional allowance was tried, to fold the interpreter's tiny leaf helpers
// (vm_base_ptr, rb_simple_iseq_p, ...) into vm_exec_core. It measurably REGRESSED
// call-heavy code (~2%): the extra bodies raise register pressure in the irreducible
// computed-goto mesh faster than they save call overhead, so the allocator spills
// more. Inlining into that mesh is gated on a register allocator that can absorb it,
// which cg12 does not yet have; until then, keep the conservative flat allowance.
func inlineGrowthCap(initial int) int {
	return initial + 128 // small absolute growth; conservative until frequency-aware
}

// inlineCallerInstrBudget stops the general inliner from growing already-large
// functions. Very large functions are exactly where cloning even tiny helpers
// can push the rest of the optimization pipeline over its memory budget.
const inlineCallerInstrBudget = 2000

// Inline replaces direct calls to small, non-recursive module functions with a
// clone of the callee's body. It is the pipeline's only transform that reasons
// across functions; every other pass runs afterward, intraprocedurally, over
// the larger bodies inlining produces.
//
// Callees are processed bottom-up over the call graph's SCC condensation, so a
// function is finalized before its callers inline it (giving accurate size and
// one-pass transitive inlining). Recursion — direct or mutual — is detected as
// SCC membership and never inlined, which guarantees termination without relying
// on the size budget.
func Inline(m *ir.Module) bool {
	done := false
	return inlineModule(m, map[*ir.Func]int{}, &done)
}

// InlinePass returns a module pass that inlines with a growth cap fixed to each
// function's size when the pass first saw it. The pipeline runs inlining in a
// fixpoint with cleanup; a fresh InlinePass per fixpoint keeps the cap from being
// recomputed against the already-grown body each round (which would let a single-site
// cascade compound past the cap).
func InlinePass() Pass {
	base := map[*ir.Func]int{}
	costDone := false
	return ModulePass("inline", func(m *ir.Module) bool { return inlineModule(m, base, &costDone) })
}

// forceInlineFromEnv marks the functions named in CG12_FORCE_INLINE (comma-
// separated) as CostInline, standing in for the cost model that will eventually
// pick hot helpers (e.g. vm_sendish) to inline into their hot callers. Using
// CostInline rather than ForceInline keeps always_inline handling untouched.
// Idempotent; a no-op when the variable is unset.
func forceInlineFromEnv(m *ir.Module) {
	list := os.Getenv("CG12_FORCE_INLINE")
	if list == "" {
		return
	}
	names := map[string]bool{}
	for _, n := range strings.Split(list, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names[n] = true
		}
	}
	for _, f := range m.Funcs {
		if names[f.Name] {
			f.CostInline = true
		}
	}
}

// selectCostInline is the inliner's cost model: it marks small static helper functions
// CostInline so the (committed) boosted splicing folds them cap-free into their hot callers
// -- the interpreter fast-path helpers (Ruby's vm_sendish, vm_opt_*, ...) into vm_exec_core,
// which cg12's flat size budget cannot reach. It targets DISPATCH callers (a computed goto is
// an interpreter's threaded dispatch loop -- all of it hot): the static frequency estimator
// cannot rank the handlers within such a loop, so, like gcc, the model selects by SIZE up to
// a per-caller growth cap. The register allocator absorbs the inlined bodies without extra
// spilling (measured: spills stay flat), so the cap bounds only icache growth, not pressure.
// CG12_NO_COSTINLINE disables it; CG12_FORCE_INLINE overrides it.
func selectCostInline(m *ir.Module, cg *callGraph, scc *sccInfo) {
	if os.Getenv("CG12_NO_COSTINLINE") != "" {
		return
	}
	dispatch := func(f *ir.Func) bool {
		for _, b := range f.Blocks {
			if b.Jmp.Kind == ir.JmpBr {
				return true
			}
		}
		return false
	}
	candidate := func(c *ir.Func) bool {
		return !c.NoInline && !c.ForceInline && !c.CostInline && !scc.recursive[c] &&
			inlinableStructure(c) && funcSize(c) > inlineSmallBudget && funcSize(c) <= maxCostInlineSize
	}

	// The static frequency estimator cannot rank a computed-goto interpreter's handlers --
	// they are all in the one hot dispatch loop, so it scores each ~1.0. So, like gcc, select
	// by SIZE: fold every small static helper called from a dispatch loop into it, smallest
	// first, up to a per-caller growth cap. Collect per (dispatch) caller its candidate
	// callees and how many sites each has.
	dump := os.Getenv("CG12_DUMP_COSTINLINE") != ""
	type cand struct {
		callee *ir.Func
		sites  int
	}
	byCaller := map[*ir.Func]map[*ir.Func]int{}
	// Callers in module order, so the loop below -- and the dump it can print --
	// does not run in map order. The selection itself is per caller and marking a
	// callee is idempotent, so this is for reproducible diagnostics rather than
	// for the result.
	var callers []*ir.Func
	for _, f := range m.Funcs {
		if f.Start == nil || !dispatch(f) {
			continue
		}
		for _, b := range f.Blocks {
			for i := range b.Instrs {
				if b.Instrs[i].Op != ir.OCall {
					continue
				}
				if c := directCallee(f, &b.Instrs[i], cg.byName); c != nil && candidate(c) {
					if byCaller[f] == nil {
						byCaller[f] = map[*ir.Func]int{}
						callers = append(callers, f)
					}
					byCaller[f][c]++
				}
			}
		}
	}
	for _, caller := range callers {
		callees := byCaller[caller]
		cands := make([]cand, 0, len(callees))
		for c, n := range callees {
			cands = append(cands, cand{c, n})
		}
		// Ties break on name. The slice above comes out of a map, and a sort that
		// leaves equal-sized candidates in input order therefore ranks them
		// differently on every compile -- which decides who gets inlined when the
		// budget runs out, and so which code the program ends up holding.
		sort.Slice(cands, func(i, j int) bool {
			leftSize, rightSize := funcSize(cands[i].callee), funcSize(cands[j].callee)
			if leftSize != rightSize {
				return leftSize < rightSize
			}
			return cands[i].callee.Name < cands[j].callee.Name
		})
		budget := funcSize(caller) * costInlineGrowthPct / 100
		for _, cd := range cands {
			grow := funcSize(cd.callee) * cd.sites // rough per-caller body growth
			if grow > budget {
				continue
			}
			cd.callee.CostInline = true
			budget -= grow
			if dump {
				fmt.Fprintf(os.Stderr, "COSTINLINE %-38s size=%-4d sites=%-3d into=%s\n",
					cd.callee.Name, funcSize(cd.callee), cd.sites, caller.Name)
			}
		}
	}
}

func inlineModule(m *ir.Module, base map[*ir.Func]int, costDone *bool) bool {
	forceInlineFromEnv(m)
	cg := buildCallGraph(m)
	scc := computeSCC(cg)
	if !*costDone {
		selectCostInline(m, cg, scc) // once, on the original graph, before any inlining
		*costDone = true
	}
	sites := callSiteCounts(m, cg.byName)
	changed := false
	for _, f := range scc.order {
		if f.Start == nil || hasSecondaryEntry(f) {
			continue
		}
		if frameIsSpentFromTheNoSplitReserve(f) {
			continue
		}
		if _, ok := base[f]; !ok {
			base[f] = funcSize(f)
		}
		if inlineInto(f, cg, scc, sites, base) {
			changed = true
		}
	}
	return changed
}

// frameIsSpentFromTheNoSplitReserve reports whether growing this function's frame
// spends stack the runtime has already promised to something else, in which case
// [inlineModule] leaves it alone.
//
// A nosplit function emits no stack-growth guard, so its frame is spent out of a
// fixed reserve the runtime keeps below g.stackguard0 -- and so is the frame of
// every nosplit function it goes on to call, until the next guarded call. Nothing
// in cg12 measures that run. Go's linker does (`nosplit stack over 792 byte
// limit` is a link-time error there); opt.AuditNoSplitCalls checks the different
// question of whether a nosplit function calls a splittable one at all. So the
// only thing keeping a cg12 nosplit chain inside the reserve is that nobody grows
// those frames, which was true for as long as `goc -O` meant fold/copy/dce.
//
// It stopped being true the moment DefaultPipeline became the default.
// runtime.mcache.nextFree -- which goc marks nosplit itself, in
// runtimeImplicitNoSplit, because upstream Go inlines it into mallocgc and cg12
// does not -- went from a 384-byte frame to a 656-byte one when the inliner
// folded its callees in. That put the nextFree/refill nosplit run at 976 bytes
// against a reserve of about 800, and goc/testdata/runtime_lock_osthread.go died
// with "fatal error: runtime: split stack overflow" on 14 runs in 100, inside
// LockOSThread -> newm -> allocm -> mallocgc -> nextFree -> refill. Intermittent
// because the chain is only entered when the mcache actually needs a refill.
//
// Declining to inline into a nosplit caller is the conservative rule, and it is
// the one that can be stated without a frame-size model: the inliner works on IR
// and cannot see a frame.
//
// cg12 now has the model. [InlineIntoNoSplitCallers] runs at the end of
// [DefaultPipeline], asks the backend how many bytes of stack each nosplit
// chain has left, inlines, measures the frame it produced, and keeps the result
// only if it fits. So this rule is no longer the last word on nosplit callers --
// it is the rule for the *unbounded* fixpoint above, which iterates inlining and
// simplification without measuring anything and must therefore keep its hands
// off frames that are spent from a reserve.
//
// What the measured pass found is that the reserve was already overspent. The
// nosplit chain through nextFree is 1104 bytes against a 920-byte reserve with
// nothing inlined into it at all; the crash this comment describes was that
// chain being pushed further, not that chain being created. nextFree still gets
// no inlining, and now for a stated reason with a number attached rather than
// because the inliner cannot see. arm64/nosplit_debt.go has the rest of the
// list; CCWORK_REPORT.md has the measurements.
//
// InlineNoSplitCalls is deliberately not covered by this. It exists for the
// opposite reason -- a signal-entry path that must not reach a stack check at all
// -- and inlines only the small accessors that removes a check from.
func frameIsSpentFromTheNoSplitReserve(function *ir.Func) bool {
	return function.NoSplit
}

// callSiteCounts returns, per module function, the number of direct call sites that
// name it -- the signal for whether inlining it duplicates code (many sites) or just
// relocates it (one site, after which the original is dead).
func callSiteCounts(m *ir.Module, byName map[string]*ir.Func) map[*ir.Func]int {
	n := map[*ir.Func]int{}
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			for i := range b.Instrs {
				if in := &b.Instrs[i]; in.Op == ir.OCall {
					if g := directCallee(f, in, byName); g != nil {
						n[g]++
					}
				}
			}
		}
	}
	return n
}

// funcSize is a function's body size in IR instructions, the inliner's size proxy.
func funcSize(f *ir.Func) int {
	n := 0
	for _, b := range f.Blocks {
		n += len(b.Instrs)
	}
	return n
}

// InlineNoSplitCalls inlines small split-stack callees into nosplit callers.
// Runtime signal entry functions can execute briefly on a signal stack while
// the G register still describes the interrupted goroutine. A stack check in
// that window would compare unrelated stack bounds, so the standard Go
// compiler relies on inlining tiny accessors used by those paths.
func InlineNoSplitCalls(m *ir.Module) bool {
	cg := buildCallGraph(m)
	scc := computeSCC(cg)
	changed := false
	for _, caller := range scc.order {
		if caller.Start == nil || !caller.NoSplit || hasSecondaryEntry(caller) {
			continue
		}
		for count := 0; count < inlineFuncBudget; count++ {
			block, index, callee := findSplitCalleeToInline(caller, cg, scc)
			if callee == nil {
				break
			}
			spliceCall(caller, block, index, callee, cg, scc, 0)
			changed = true
		}
	}
	return changed
}

// InlineHeapAllocations exposes small constructor allocations to their callers
// before escape lowering. A constructor may need to return a heap candidate in
// isolation, while a caller that immediately copies or reads the value can keep
// the same object on its stack. Keeping this as a focused pre-escape pass avoids
// requiring the full optimization pipeline for ordinary Go compilation.
func InlineHeapAllocations(module *ir.Module) bool {
	graph := buildCallGraph(module)
	components := computeSCC(graph)
	changed := false
	for _, caller := range components.order {
		if caller.Start == nil || hasSecondaryEntry(caller) {
			continue
		}
		for count := 0; count < inlineFuncBudget; count++ {
			block, index, callee := findHeapAllocatingConstructor(caller, graph, components)
			if callee == nil {
				break
			}
			spliceCall(caller, block, index, callee, graph, components, 0)
			changed = true
		}
	}

	graph = buildCallGraph(module)
	components = computeSCC(graph)
	for _, caller := range components.order {
		if caller.Start == nil || hasSecondaryEntry(caller) {
			continue
		}
		for count := 0; count < inlineFuncBudget; count++ {
			block, index, callee := findHeapAllocationConsumer(caller, graph, components)
			if callee == nil {
				break
			}
			spliceCall(caller, block, index, callee, graph, components, 0)
			changed = true
		}
	}
	return changed
}

func findHeapAllocatingConstructor(caller *ir.Func, graph *callGraph, components *sccInfo) (*ir.Block, int, *ir.Func) {
	for _, block := range caller.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.Op != ir.OCall {
				continue
			}
			callee := directCallee(caller, instruction, graph.byName)
			if callee == nil || components.recursive[callee] || !canInlineCall(caller, instruction, callee) {
				continue
			}
			if len(instruction.Args)-1 != len(callee.Params) {
				continue
			}
			if !containsHeapAllocation(callee) {
				continue
			}
			if inlinable(callee) {
				return block, index, callee
			}
			if callee.RetAgg != nil && callee.RetValues &&
				inlinableWithBudget(callee, preEscapeAggregateInstrBudget) {
				return block, index, callee
			}
		}
	}
	return nil, 0, nil
}

func findHeapAllocationConsumer(caller *ir.Func, graph *callGraph, components *sccInfo) (*ir.Block, int, *ir.Func) {
	for _, block := range caller.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.Op != ir.OCall {
				continue
			}
			callee := directCallee(caller, instruction, graph.byName)
			if callee == nil || components.recursive[callee] || !canInlineCall(caller, instruction, callee) {
				continue
			}
			if len(instruction.Args)-1 != len(callee.Params) {
				continue
			}
			if callee.RetAgg == nil || !callee.RetValues {
				continue
			}
			if callUsesHeapAllocation(caller, instruction) &&
				inlinableWithBudget(callee, preEscapeInstrBudget) {
				return block, index, callee
			}
		}
	}
	return nil, 0, nil
}

func callUsesHeapAllocation(function *ir.Func, call *ir.Instr) bool {
	heapValues := make(map[uint32]bool)
	heapSlots := make(map[localSlot]bool)
	aliases := newAliasInfo(function)
	for changed := true; changed; {
		changed = false
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				if instruction.To.Kind == ir.RefTemp && !heapValues[instruction.To.ID] {
					derived := instruction.Op == ir.OHeapAlloc
					if heapValueDerivation(instruction, heapValues) {
						derived = true
					}
					if instruction.Op.IsLoad() {
						location := aliases.locOf(instruction.Arg(0), 1)
						if location.class == cLocal {
							slot := localSlot{base: location.key(), offset: location.offset}
							if heapSlots[slot] {
								derived = true
							}
						}
					}
					if derived {
						heapValues[instruction.To.ID] = true
						changed = true
					}
				}

				if !instruction.Op.IsStore() || !isHeapValue(instruction.Arg(0), heapValues) {
					continue
				}
				location := aliases.locOf(instruction.Arg(1), 1)
				if location.class != cLocal {
					continue
				}
				slot := localSlot{base: location.key(), offset: location.offset}
				if !heapSlots[slot] {
					heapSlots[slot] = true
					changed = true
				}
			}
		}
	}
	for _, argument := range call.Args[1:] {
		if argument.Kind == ir.RefTemp && heapValues[argument.ID] {
			return true
		}
	}
	return false
}

func heapValueDerivation(instruction ir.Instr, heapValues map[uint32]bool) bool {
	if instruction.Op == ir.OCopy || instruction.Op == ir.OCast {
		return isHeapValue(instruction.Arg(0), heapValues)
	}
	if (instruction.Op != ir.OAdd && instruction.Op != ir.OSub) || instruction.Cls != ir.ClsP {
		return false
	}
	if isHeapValue(instruction.Arg(0), heapValues) {
		return true
	}
	if instruction.Op == ir.OAdd && isHeapValue(instruction.Arg(1), heapValues) {
		return true
	}
	return false
}

func isHeapValue(reference ir.Ref, heapValues map[uint32]bool) bool {
	return reference.Kind == ir.RefTemp && heapValues[reference.ID]
}

func containsHeapAllocation(function *ir.Func) bool {
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.Op == ir.OHeapAlloc {
				return true
			}
		}
	}
	return false
}

func findSplitCalleeToInline(caller *ir.Func, cg *callGraph, scc *sccInfo) (*ir.Block, int, *ir.Func) {
	for _, block := range caller.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.Op != ir.OCall {
				continue
			}
			callee := directCallee(caller, instruction, cg.byName)
			if callee == nil || callee.NoSplit || scc.recursive[callee] || !canInlineCall(caller, instruction, callee) {
				continue
			}
			if len(instruction.Args)-1 != len(callee.Params) {
				continue
			}
			if inlinable(callee) {
				return block, index, callee
			}
		}
	}
	return nil, 0, nil
}

// inlineInto repeatedly inlines eligible calls in caller until none remain or
// the per-function budget is spent. Re-scanning after each splice lets calls
// exposed by a previous inline (the callee's own calls) be inlined in turn; the
// budget bounds code-size growth (e.g. a diamond call graph inlined repeatedly).
func inlineInto(caller *ir.Func, cg *callGraph, scc *sccInfo, sites map[*ir.Func]int, base map[*ir.Func]int) bool {
	if inlineCallerOverBudget(caller) {
		return false
	}
	changed := false

	// A CostInline callee (the cost model's choice, e.g. vm_sendish) is big -- it blows
	// the growth cap on its own -- but must not consume the discretionary headroom the
	// budgeted pass needs, or it evicts the small hot helpers that pass folds in (measured:
	// inlining vm_sendish into vm_exec_core left vm_get_ep/... as calls, ~2x slower). A
	// caller that receives one goes "boosted": its CostInline AND its whole always_inline
	// subtree (the small helpers the big inline pulls in) are spliced first, cap-free, and
	// their growth folded into the budget anchor -- so the budgeted pass spends its headroom
	// entirely on the small NON-forced hot helpers (vm_get_ep, ...). A caller with no
	// CostInline (every default build, until the cost model exists) is never boosted, so its
	// always_inline stay in the budgeted pass and it inlines byte-identically to before.
	boosted := false
	for _, b := range caller.Blocks {
		for i := range b.Instrs {
			if c := directCallee(caller, &b.Instrs[i], cg.byName); c != nil && c.CostInline {
				boosted = true
			}
		}
	}
	forced := func(c *ir.Func) bool { return c.CostInline }
	if boosted {
		forced = func(c *ir.Func) bool { return c.CostInline || c.ForceInline }
	}

	before := funcSize(caller)
	for n := 0; n < inlineFuncBudget; n++ {
		if inlineCallerOverBudget(caller) {
			break
		}
		b, idx, callee := findInlinable(caller, cg, scc, sites, forced)
		if callee == nil {
			break
		}
		spliceCall(caller, b, idx, callee, cg, scc, 0)
		changed = true
	}
	base[caller] += funcSize(caller) - before

	cap := inlineGrowthCap(base[caller])
	for n := 0; n < inlineFuncBudget; n++ {
		if inlineCallerOverBudget(caller) || funcSize(caller) > cap {
			break // this function has grown enough; stop feeding the cascade
		}
		b, idx, callee := findInlinable(caller, cg, scc, sites, func(c *ir.Func) bool { return !forced(c) })
		if callee == nil {
			break
		}
		spliceCall(caller, b, idx, callee, cg, scc, 0)
		changed = true
	}
	return changed
}

func inlineCallerOverBudget(function *ir.Func) bool {
	return functionInstructionCount(function) > inlineCallerInstrBudget
}

func functionInstructionCount(function *ir.Func) int {
	count := 0
	for _, block := range function.Blocks {
		count += len(block.Phis) + len(block.Instrs)
	}
	return count
}

// findInlinable returns the first call in caller worth inlining that the accept
// predicate admits: a direct call to a non-recursive module function the cost model
// accepts.
func findInlinable(caller *ir.Func, cg *callGraph, scc *sccInfo, sites map[*ir.Func]int, accept func(*ir.Func) bool) (*ir.Block, int, *ir.Func) {
	for _, b := range caller.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op != ir.OCall {
				continue
			}
			callee := directCallee(caller, in, cg.byName)
			if callee == nil || scc.recursive[callee] || !canInlineCall(caller, in, callee) {
				continue // indirect/external, or part of a recursion cycle
			}
			if !accept(callee) {
				continue // this pass (large-forced vs budgeted) is not handling this callee
			}
			if len(in.Args)-1 != len(callee.Params) {
				continue
			}
			if worthInlining(callee, sites[callee]) {
				return b, i, callee
			}
		}
	}
	return nil, 0, nil
}

// directCallee returns the module function a call names directly, or nil for an
// indirect or external call.
func directCallee(caller *ir.Func, call *ir.Instr, byName map[string]*ir.Func) *ir.Func {
	c := call.Arg(0)
	if c.Kind != ir.RefConst || caller.Consts[c.ID].Kind != ir.ConstSym {
		return nil
	}
	return byName[caller.Consts[c.ID].Sym]
}

func canInlineCall(caller *ir.Func, call *ir.Instr, callee *ir.Func) bool {
	if isFrameScopedRuntimeCall(caller, call) {
		return false
	}
	if !callee.HasClosureContext {
		return true
	}
	return call.ClosureCall && !call.ClosureContext.IsNone()
}

// inlinableStructure reports whether callee can be inlined mechanically: it has a
// body and a return to splice into, and neither a variadic signature nor a by-value
// aggregate parameter or result (which the splice does not model). Recursion is not
// checked here -- it is a call-graph property handled by the SCC classification in
// findInlinable. Whether an inlinable callee is worth inlining is a separate cost
// decision (worthInlining).
func inlinableStructure(callee *ir.Func) bool {
	if callee.Start == nil || hasSecondaryEntry(callee) || callee.Variadic {
		return false
	}
	// An aggregate-returning callee is inlinable: spliceCall materializes the
	// copy-at-return the ABI would otherwise emit (see aggCopy). CG12_NO_AGGINLINE
	// disables it for bisection.
	if callee.RetAgg != nil && os.Getenv("CG12_NO_AGGINLINE") != "" {
		return false
	}
	for _, p := range callee.Params {
		if p.Agg != nil {
			return false // by-value aggregate parameter (Stage B)
		}
	}
	hasRet := false
	for _, b := range callee.Blocks {
		// A computed goto (JmpBr) or a taken label address (OBlockAddr) makes the
		// body's control flow depend on block addresses; splicing it into a caller,
		// where those addresses move, is unsound, so leave it a call -- as gcc does
		// for a function containing an address-of-label.
		if b.Jmp.Kind == ir.JmpBr {
			return false
		}
		for k := range b.Instrs {
			if b.Instrs[k].Op == ir.OBlockAddr {
				return false
			}
			if isFrameScopedRuntimeCall(callee, &b.Instrs[k]) || isCallerFrameIntrinsic(&b.Instrs[k]) {
				return false
			}
		}
		if b.Jmp.Kind == ir.JmpRet {
			hasRet = true
		}
	}
	return hasRet // needs a return to splice the continuation onto
}

func inlinable(callee *ir.Func) bool {
	return inlinableWithBudget(callee, inlineSmallBudget)
}

func inlinableWithBudget(callee *ir.Func, instructionBudget int) bool {
	if !inlinableStructure(callee) {
		return false
	}
	return funcSize(callee) <= instructionBudget
}

// isCallerFrameIntrinsic reports operations whose result names the physical
// caller's frame. Inlining their containing function would change that caller
// and therefore change program semantics. This is the same restriction used by
// the Go compiler for internal/runtime/sys.GetCallerPC and GetCallerSP.
func isCallerFrameIntrinsic(instruction *ir.Instr) bool {
	if instruction.Op != ir.OIntrinsic || instruction.Intrin == nil {
		return false
	}
	switch instruction.Intrin.Name {
	case "getcallerpc", "getcallersp":
		return true
	default:
		return false
	}
}

func isFrameScopedRuntimeCall(function *ir.Func, instruction *ir.Instr) bool {
	if instruction.Op != ir.OCall || len(instruction.Args) == 0 {
		return false
	}
	callee := instruction.Arg(0)
	if callee.Kind != ir.RefConst {
		return false
	}
	constant := function.Consts[callee.ID]
	if constant.Kind != ir.ConstSym {
		return false
	}
	return function.Module().SymAttrOf(constant.Sym).Has(ir.SymFrameScoped)
}

// worthInlining applies the cost model to a structurally-inlinable callee named at
// `sites` call sites across the module. A forced callee always inlines. Otherwise the
// size budget is inlineOnceBudget/sites (floored at inlineSmallBudget): a lone site
// inlines a large body cheaply because the original is then dead, while a body copied
// into many sites must be small to be worth the duplication.
func worthInlining(callee *ir.Func, sites int) bool {
	// noinline/cold is honored before everything else, so a cold slow-path helper is
	// never inlined -- it stays a call, out of the hot inlined body (gcc's hot/cold
	// split). This wins over ForceInline: the two attributes are mutually exclusive,
	// but if both were somehow set, "do not inline" is the safe resolution.
	if callee.NoInline {
		return false
	}
	if !inlinableStructure(callee) {
		return false
	}
	if callee.ForceInline || callee.CostInline {
		return true // mandatory (always_inline) or cost-model-chosen: size budget bypassed
	}
	if sites < 1 {
		sites = 1
	}
	budget := inlineOnceBudget / sites
	if budget < inlineSmallBudget {
		budget = inlineSmallBudget
	}
	return funcSize(callee) <= budget
}

// spliceCall replaces the call at caller block b, instruction idx, with a clone
// of callee's body. The call's block is split at the call: pre-call code stays,
// the callee's entry is jumped into, its returns jump to a continuation block
// holding the post-call code, and the call's result becomes a phi of the
// returned values.
//
// When callee is recursive, the cloned calls that continue the same recursion
// cycle are stamped with depth+1 in their Aux slot, so bounded unrolling
// (UnrollRecursion) can tell how deep it has gone and stop. For a non-recursive
// callee nothing is stamped and depth is ignored.
func spliceCall(caller *ir.Func, b *ir.Block, idx int, callee *ir.Func, cg *callGraph, scc *sccInfo, depth int) {
	call := b.Instrs[idx]
	args := call.Args[1:]
	results := []ir.Ref{call.To}
	results = append(results, call.Defs...)

	// An aggregate-returning callee returns a pointer to its value; the call's
	// result is a pointer to a buffer holding a copy of it. Materialize that buffer
	// and copy at each return (below), reproducing what the return ABI would emit.
	aggRet := callee.RetAgg
	copyAggregateReturn := aggRet != nil && !callee.RetValues
	aggSize := 0
	if copyAggregateReturn {
		aggSize, _ = aggRet.Layout()
	}

	// Snapshot the callee's body before mutating anything. The callee may BE the
	// caller (self-recursion unrolling), in which case its temp/const/block
	// slices alias the caller's and grow as we clone; the snapshot keeps the
	// clone reading the original body (including block b's pre-split contents).
	type blkSnap struct {
		orig   *ir.Block
		phis   []*ir.Phi
		instrs []ir.Instr
		jmp    ir.Jmp
	}
	body := make([]blkSnap, len(callee.Blocks))
	for i, cb := range callee.Blocks {
		body[i] = blkSnap{cb, cb.Phis, append([]ir.Instr(nil), cb.Instrs...), cb.Jmp}
	}
	entry := callee.Start
	nTemps, nConsts := len(callee.Temps), len(callee.Consts)

	tempMap := make([]ir.Ref, nTemps)
	isParam := make([]bool, nTemps)
	closureContext := inlineClosureContext(caller, call)
	for k, p := range callee.Params {
		tempMap[p.ID] = args[k] // a parameter becomes its argument, in the caller's terms
		isParam[p.ID] = true
	}
	for id := 0; id < nTemps; id++ {
		if t := callee.Temps[id]; t != nil && !isParam[id] {
			if t.ClosureContext && call.ClosureCall {
				tempMap[id] = closureContext
				continue
			}
			mapped := caller.NewTemp(t.Name, t.Cls)
			cloned := caller.Temp(mapped)
			cloned.Agg = t.Agg
			cloned.GCRef = t.GCRef
			cloned.GCType = t.GCType
			cloned.Fixed = t.Fixed
			cloned.Reg = t.Reg
			tempMap[id] = mapped
		}
	}
	for id, words := range callee.StackPointerWords {
		if int(id) >= nTemps {
			continue
		}
		mapped := tempMap[id]
		if mapped.Kind != ir.RefTemp {
			continue
		}
		if caller.StackPointerWords == nil {
			caller.StackPointerWords = make(map[uint32]map[int]bool)
		}
		clonedWords := make(map[int]bool, len(words))
		for offset, pointer := range words {
			clonedWords[offset] = pointer
		}
		caller.StackPointerWords[mapped.ID] = clonedWords
	}
	// The front end's placement records follow their allocation into the caller,
	// the same way the pointer-word map does. A constructor inlined into three
	// callers is three allocations, each decided separately, so each copy keeps
	// the record of what the front end chose for it.
	for id, placed := range callee.PlacedAllocs {
		if int(id) >= nTemps {
			continue
		}
		mapped := tempMap[id]
		if mapped.Kind != ir.RefTemp {
			continue
		}
		if caller.PlacedAllocs == nil {
			caller.PlacedAllocs = make(map[uint32]ir.PlacedAlloc)
		}
		caller.PlacedAllocs[mapped.ID] = placed
	}
	constMap := make([]ir.Ref, nConsts)
	for i := 0; i < nConsts; i++ {
		constMap[i] = cloneConst(caller, callee.Consts[i])
	}
	mapRef := func(r ir.Ref) ir.Ref {
		switch r.Kind {
		case ir.RefTemp:
			return tempMap[r.ID]
		case ir.RefConst:
			return constMap[r.ID]
		}
		return r // absent, or a module-shared aggregate-type ref
	}

	// Continuation block: everything after the call, plus the original
	// terminator. The call itself is dropped. Cloned blocks take fresh
	// auto-generated names (NewBlock("")) so that names — which the arm64
	// emitter turns into assembler labels — stay unique across inlinings.
	cont := caller.NewBlock("")
	cont.Instrs = append(cont.Instrs, b.Instrs[idx+1:]...)
	cont.Jmp = b.Jmp
	b.Instrs = b.Instrs[:idx:idx]

	// The aggregate result buffer is allocated in the pre-call block, which
	// dominates every cloned block and cont, so it is a legal definition of result.
	if copyAggregateReturn && !call.To.IsNone() {
		b.Instrs = append(b.Instrs, ir.Instr{
			Op: ir.OAlloc16, Cls: ir.ClsL, To: call.To,
			Args: []ir.Ref{caller.Long(int64(aggSize))},
		})
	}

	// Clone the snapshotted blocks, then their contents (remapping every value
	// and block reference), turning returns into jumps to the continuation.
	blockMap := make(map[*ir.Block]*ir.Block, len(body))
	for _, s := range body {
		blockMap[s.orig] = caller.NewBlock("")
	}
	// mapBlock re-points a block reference (an OBlockAddr's &&label target) at its
	// clone, so an inlined computed-goto pad names the copied block, not the callee's
	// original -- which after inlining is unreachable and dropped, leaving a dangling
	// address. A reference outside the callee body is left as is (should not occur).
	mapBlock := func(b *ir.Block) *ir.Block {
		if nb, ok := blockMap[b]; ok {
			return nb
		}
		return b
	}
	// Inline provenance: cloned instructions record that they came from callee,
	// called at this call's position, nested under the call's own inline context.
	site := &ir.InlineSite{Callee: callee.Name, Call: call.Pos, Parent: call.Inl}
	rebaseCache := map[*ir.InlineSite]*ir.InlineSite{}

	retVals := make([][]ir.Ref, len(results))
	var retBlocks []*ir.Block
	for _, s := range body {
		tb := blockMap[s.orig]
		for _, ph := range s.phis {
			np := &ir.Phi{Cls: ph.Cls, To: mapRef(ph.To)}
			for k := range ph.Args {
				np.Args = append(np.Args, mapRef(ph.Args[k]))
				np.Blocks = append(np.Blocks, blockMap[ph.Blocks[k]])
			}
			tb.Phis = append(tb.Phis, np)
		}
		for i := range s.instrs {
			cl := cloneInstr(caller, &s.instrs[i], mapRef, mapBlock)
			cl.Inl = rebaseInline(s.instrs[i].Inl, site, rebaseCache)
			if cl.Op == ir.OCall {
				// After inlining, control returns to cont, so a call that was in tail
				// position in the callee no longer is; a stray Tail flag would make
				// tail-call lowering reject it.
				cl.Tail = false
			}
			tb.Instrs = append(tb.Instrs, cl)
		}
		switch s.jmp.Kind {
		case ir.JmpRet:
			switch {
			case copyAggregateReturn:
				// Snapshot the returned aggregate into the result buffer.
				if !call.To.IsNone() && !s.jmp.Arg.IsNone() {
					aggCopy(caller, call.To, mapRef(s.jmp.Arg), aggSize, &tb.Instrs)
				}
			case len(results) > 0 && !results[0].IsNone() && !s.jmp.Arg.IsNone():
				returned := []ir.Ref{s.jmp.Arg}
				returned = append(returned, s.jmp.Args...)
				if len(returned) != len(results) {
					panic("opt: inlined return does not match call results")
				}
				for resultIndex, value := range returned {
					retVals[resultIndex] = append(retVals[resultIndex], mapRef(value))
				}
				retBlocks = append(retBlocks, tb)
			}
			tb.Jmp = ir.Jmp{Kind: ir.JmpJmp, To: cont}
		case ir.JmpJmp:
			tb.Jmp = ir.Jmp{Kind: ir.JmpJmp, To: blockMap[s.jmp.To]}
		case ir.JmpJnz:
			tb.Jmp = ir.Jmp{Kind: ir.JmpJnz, Arg: mapRef(s.jmp.Arg), To: blockMap[s.jmp.To], To2: blockMap[s.jmp.To2]}
		case ir.JmpSwitch:
			nc := make([]ir.SwitchCase, len(s.jmp.Cases))
			for k, c := range s.jmp.Cases {
				nc[k] = ir.SwitchCase{Val: c.Val, Blk: blockMap[c.Blk]}
			}
			tb.Jmp = ir.Jmp{Kind: ir.JmpSwitch, Arg: mapRef(s.jmp.Arg), To: blockMap[s.jmp.To], Signed: s.jmp.Signed, Cases: nc}
		case ir.JmpBr:
			nt := make([]*ir.Block, len(s.jmp.Targets))
			for k, t := range s.jmp.Targets {
				nt[k] = blockMap[t]
			}
			tb.Jmp = ir.Jmp{Kind: ir.JmpBr, Arg: mapRef(s.jmp.Arg), Targets: nt}
		case ir.JmpHlt:
			tb.Jmp = ir.Jmp{Kind: ir.JmpHlt}
		}
	}

	// Enter the inlined body.
	b.Jmp = ir.Jmp{Kind: ir.JmpJmp, To: blockMap[entry]}

	// Preserve a single return as a copy so alias and escape passes can see
	// through it. Multiple return paths still require a phi. A non-scalar
	// aggregate result was placed in its buffer at each return above, so it needs
	// no phi.
	if len(results) > 0 && !results[0].IsNone() && !copyAggregateReturn {
		for resultIndex, result := range results {
			values := retVals[resultIndex]
			if len(values) == 1 {
				copyResult := ir.Instr{
					Op:   ir.OCopy,
					Cls:  caller.ClassOf(result),
					To:   result,
					Args: []ir.Ref{values[0]},
					Pos:  call.Pos,
					Inl:  call.Inl,
				}
				cont.Instrs = append([]ir.Instr{copyResult}, cont.Instrs...)
			} else {
				phi := &ir.Phi{Cls: caller.ClassOf(result), To: result, Args: values, Blocks: retBlocks}
				cont.Phis = append(cont.Phis, phi)
			}
		}
	}

	// The original terminator now lives in cont, so successors that named b as a
	// phi predecessor must name cont instead.
	for _, s := range cont.Succs() {
		for _, ph := range s.Phis {
			for k := range ph.Blocks {
				if ph.Blocks[k] == b {
					ph.Blocks[k] = cont
				}
			}
		}
	}

	// Stamp the cloned calls that continue this recursion cycle with the next
	// depth, so bounded unrolling knows when to stop. (The blocks are final now,
	// so &tb.Instrs[i] is stable.)
	if scc.recursive[callee] {
		cycle := scc.comp[callee]
		for _, s := range body {
			tb := blockMap[s.orig]
			for i := range tb.Instrs {
				in := &tb.Instrs[i]
				if in.Op != ir.OCall {
					continue
				}
				if g := directCallee(caller, in, cg.byName); g != nil && scc.comp[g] == cycle {
					in.Unroll = int32(depth + 1)
				}
			}
		}
	}
}

// aggCopy appends chunked load/store instructions copying size bytes from src to
// dst, the same lowering the aggregate-return ABI uses (arm64/amd64 emitMemcpy) but
// at the target-independent IR level. Splicing an aggregate-returning callee needs
// it: the callee returns a pointer to its value, and C's copy-at-return semantics
// require snapshotting that value into the caller's result buffer at the return
// point (the pointee may be caller-reachable memory mutated after the call). The
// later SROA/forwarding pass collapses these chunks back to registers where it can.
func aggCopy(f *ir.Func, dst, src ir.Ref, size int, out *[]ir.Instr) {
	off := 0
	chunk := func(w int, loadOp, storeOp ir.Op, cls ir.Cls) {
		for size-off >= w {
			s := aggOffset(f, src, off, out)
			d := aggOffset(f, dst, off, out)
			v := f.NewTemp("", cls)
			*out = append(*out, ir.Instr{Op: loadOp, Cls: cls, To: v, Args: []ir.Ref{s}})
			*out = append(*out, ir.Instr{Op: storeOp, Cls: cls, Args: []ir.Ref{v, d}})
			off += w
		}
	}
	chunk(8, ir.OLoadl, ir.OStorel, ir.ClsL)
	chunk(4, ir.OLoaduw, ir.OStorew, ir.ClsW)
	chunk(2, ir.OLoaduh, ir.OStoreh, ir.ClsW)
	chunk(1, ir.OLoadub, ir.OStoreb, ir.ClsW)
}

// aggOffset returns a reference to base + off, appending an add when off != 0.
func aggOffset(f *ir.Func, base ir.Ref, off int, out *[]ir.Instr) ir.Ref {
	if off == 0 {
		return base
	}
	t := f.NewTemp("", ir.ClsL)
	*out = append(*out, ir.Instr{Op: ir.OAdd, Cls: ir.ClsL, To: t, Args: []ir.Ref{base, f.Long(int64(off))}})
	return t
}

func inlineClosureContext(caller *ir.Func, call ir.Instr) ir.Ref {
	context := call.ClosureContext
	if context.Kind != ir.RefTemp {
		return context
	}
	for _, block := range caller.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.To == context && instruction.Op == ir.OCopy && len(instruction.Args) == 1 {
				return instruction.Args[0]
			}
		}
	}
	return context
}

// cloneInstr copies an instruction, remapping every value reference and any block
// reference (an OBlockAddr's &&label target) into the caller's cloned body.
func cloneInstr(caller *ir.Func, in *ir.Instr, mapRef func(ir.Ref) ir.Ref, mapBlock func(*ir.Block) *ir.Block) ir.Instr {
	// Volatile is a semantic flag that must ride along: a cloned volatile store is
	// still observable. Tail is copied here too, but spliceCall clears it on the
	// clone, because a call inlined into a caller returns to the continuation and is
	// no longer in tail position. Amode is normally unset before lowering, but copy
	// it so this remains a total instruction clone if a later pipeline reorders the
	// pass.
	out := ir.Instr{
		Op:                in.Op,
		Cls:               in.Cls,
		To:                mapRef(in.To),
		Cmp:               in.Cmp,
		Aux:               in.Aux,
		Amode:             in.Amode,
		Unroll:            in.Unroll,
		RetAgg:            in.RetAgg,
		RetValues:         in.RetValues,
		StackResult:       mapRef(in.StackResult),
		StackResultOffset: in.StackResultOffset,
		Tail:              in.Tail,
		Volatile:          in.Volatile,
		ClosureCall:       in.ClosureCall,
		ClosureContext:    mapRef(in.ClosureContext),
		Asm:               in.Asm,
		Intrin:            in.Intrin,
		Pos:               in.Pos,
	}
	if in.Blk != nil {
		out.Blk = mapBlock(in.Blk)
	}
	for _, a := range in.Args {
		out.Args = append(out.Args, mapRef(a))
	}
	if out.Op == ir.OCmp && len(out.Args) > 0 && caller.ClassOf(out.Args[0]).IsFloat() {
		out.Cmp = floatingComparison(out.Cmp)
	}
	if in.AggArgs != nil {
		out.AggArgs = append([]*ir.AggType(nil), in.AggArgs...)
	}
	if in.ArgGroups != nil {
		out.ArgGroups = append([]ir.ValueGroup(nil), in.ArgGroups...)
	}
	for _, d := range in.Defs {
		out.Defs = append(out.Defs, mapRef(d))
	}
	return out
}

// floatingComparison preserves the source comparison when inlining shared
// generic code into a floating-point instantiation. The generic body may use
// an integer representative for a mixed ordered constraint; substituting a
// float argument changes the operand class and therefore also requires the
// floating-point form of its comparison predicate.
func floatingComparison(comparison ir.Cmp) ir.Cmp {
	switch comparison {
	case ir.CmpEq:
		return ir.CmpFeq
	case ir.CmpNe:
		return ir.CmpFne
	case ir.CmpSlt, ir.CmpUlt:
		return ir.CmpFlt
	case ir.CmpSle, ir.CmpUle:
		return ir.CmpFle
	case ir.CmpSgt, ir.CmpUgt:
		return ir.CmpFgt
	case ir.CmpSge, ir.CmpUge:
		return ir.CmpFge
	default:
		return comparison
	}
}

// rebaseInline re-roots a callee instruction's inline chain (relative to the
// callee body) under base, the site describing this inline. A nil chain (the
// common case: the instruction was not itself inlined) becomes base directly;
// a non-nil chain is copied with base spliced in at the bottom. Results are
// memoized so all instructions sharing an original context share one rebased
// chain (letting backends compare contexts by pointer).
func rebaseInline(orig, base *ir.InlineSite, cache map[*ir.InlineSite]*ir.InlineSite) *ir.InlineSite {
	if orig == nil {
		return base
	}
	if v, ok := cache[orig]; ok {
		return v
	}
	v := &ir.InlineSite{Callee: orig.Callee, Call: orig.Call, Parent: rebaseInline(orig.Parent, base, cache)}
	cache[orig] = v
	return v
}

// cloneConst interns a callee constant into the caller.
func cloneConst(f *ir.Func, c ir.Const) ir.Ref {
	switch c.Kind {
	case ir.ConstInt:
		return f.ConstInt(c.Cls, c.Int)
	case ir.ConstFloat:
		if c.Cls == ir.ClsS {
			return f.Single(c.Flt)
		}
		return f.Double(c.Flt)
	case ir.ConstSym:
		if c.Thread {
			// A thread-local symbol must stay thread-local when inlined, or its
			// reference is emitted with a non-TLS relocation and the link fails.
			return f.ThreadSym(c.Sym)
		}
		return f.SymClass(c.Sym, c.Int, c.Cls)
	}
	return ir.R
}
