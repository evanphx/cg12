package opt

import "github.com/evanphx/cg12/ir"

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
	return inlineModule(m, map[*ir.Func]int{})
}

// InlinePass returns a module pass that inlines with a growth cap fixed to each
// function's size when the pass first saw it. The pipeline runs inlining in a
// fixpoint with cleanup; a fresh InlinePass per fixpoint keeps the cap from being
// recomputed against the already-grown body each round (which would let a single-site
// cascade compound past the cap).
func InlinePass() Pass {
	base := map[*ir.Func]int{}
	return ModulePass("inline", func(m *ir.Module) bool { return inlineModule(m, base) })
}

func inlineModule(m *ir.Module, base map[*ir.Func]int) bool {
	cg := buildCallGraph(m)
	scc := computeSCC(cg)
	sites := callSiteCounts(m, cg.byName)
	changed := false
	for _, f := range scc.order {
		if f.Start == nil {
			continue
		}
		if _, ok := base[f]; !ok {
			base[f] = funcSize(f)
		}
		if inlineInto(f, cg, scc, sites, base[f]) {
			changed = true
		}
	}
	return changed
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

// inlineInto repeatedly inlines eligible calls in caller until none remain or
// the per-function budget is spent. Re-scanning after each splice lets calls
// exposed by a previous inline (the callee's own calls) be inlined in turn; the
// budget bounds code-size growth (e.g. a diamond call graph inlined repeatedly).
func inlineInto(caller *ir.Func, cg *callGraph, scc *sccInfo, sites map[*ir.Func]int, baseSize int) bool {
	changed := false
	cap := inlineGrowthCap(baseSize)
	for n := 0; n < inlineFuncBudget; n++ {
		if funcSize(caller) > cap {
			break // this function has grown enough; stop feeding the cascade
		}
		b, idx, callee := findInlinable(caller, cg, scc, sites)
		if callee == nil {
			break
		}
		spliceCall(caller, b, idx, callee, cg, scc, 0)
		changed = true
	}
	return changed
}

// findInlinable returns the first call in caller worth inlining: a direct call to a
// non-recursive module function the cost model accepts.
func findInlinable(caller *ir.Func, cg *callGraph, scc *sccInfo, sites map[*ir.Func]int) (*ir.Block, int, *ir.Func) {
	for _, b := range caller.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op != ir.OCall {
				continue
			}
			callee := directCallee(caller, in, cg.byName)
			if callee == nil || scc.recursive[callee] {
				continue // indirect/external, or part of a recursion cycle
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

// inlinableStructure reports whether callee can be inlined mechanically: it has a
// body and a return to splice into, and neither a variadic signature nor a by-value
// aggregate parameter or result (which the splice does not model). Recursion is not
// checked here -- it is a call-graph property handled by the SCC classification in
// findInlinable. Whether an inlinable callee is worth inlining is a separate cost
// decision (worthInlining).
func inlinableStructure(callee *ir.Func) bool {
	if callee.Start == nil || callee.Variadic || callee.RetAgg != nil {
		return false
	}
	for _, p := range callee.Params {
		if p.Agg != nil {
			return false // by-value aggregate parameter
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
		}
		if b.Jmp.Kind == ir.JmpRet {
			hasRet = true
		}
	}
	return hasRet // needs a return to splice the continuation onto
}

// worthInlining applies the cost model to a structurally-inlinable callee named at
// `sites` call sites across the module. A forced callee always inlines. Otherwise the
// size budget is inlineOnceBudget/sites (floored at inlineSmallBudget): a lone site
// inlines a large body cheaply because the original is then dead, while a body copied
// into many sites must be small to be worth the duplication.
func worthInlining(callee *ir.Func, sites int) bool {
	if !inlinableStructure(callee) {
		return false
	}
	if callee.ForceInline {
		return true
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
	result := call.To

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
	for k, p := range callee.Params {
		tempMap[p.ID] = args[k] // a parameter becomes its argument, in the caller's terms
		isParam[p.ID] = true
	}
	for id := 0; id < nTemps; id++ {
		if t := callee.Temps[id]; t != nil && !isParam[id] {
			tempMap[id] = caller.NewTemp(t.Name, t.Cls)
		}
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

	var retVals, retBlocks = []ir.Ref{}, []*ir.Block{}
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
			cl := cloneInstr(&s.instrs[i], mapRef, mapBlock)
			cl.Inl = rebaseInline(s.instrs[i].Inl, site, rebaseCache)
			tb.Instrs = append(tb.Instrs, cl)
		}
		switch s.jmp.Kind {
		case ir.JmpRet:
			if !result.IsNone() && !s.jmp.Arg.IsNone() {
				retVals = append(retVals, mapRef(s.jmp.Arg))
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

	// The call's result is the phi of the values the returns carried.
	if !result.IsNone() {
		ph := &ir.Phi{Cls: caller.ClassOf(result), To: result, Args: retVals, Blocks: retBlocks}
		cont.Phis = append(cont.Phis, ph)
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

// cloneInstr copies an instruction, remapping every value reference and any block
// reference (an OBlockAddr's &&label target) into the caller's cloned body.
func cloneInstr(in *ir.Instr, mapRef func(ir.Ref) ir.Ref, mapBlock func(*ir.Block) *ir.Block) ir.Instr {
	// Volatile and Tail are semantic flags, not scheduling hints: a cloned
	// volatile store is still observable and a cloned tail call is still in tail
	// position, so both must ride along. Amode is deliberately not copied -- it is
	// set only during lowering, after every pass that clones instructions has run,
	// so it is always zero here.
	out := ir.Instr{Op: in.Op, Cls: in.Cls, To: mapRef(in.To), Cmp: in.Cmp, Aux: in.Aux, Unroll: in.Unroll, RetAgg: in.RetAgg, Asm: in.Asm, Intrin: in.Intrin, Pos: in.Pos, Volatile: in.Volatile, Tail: in.Tail}
	if in.Blk != nil {
		out.Blk = mapBlock(in.Blk)
	}
	for _, a := range in.Args {
		out.Args = append(out.Args, mapRef(a))
	}
	if in.AggArgs != nil {
		out.AggArgs = append([]*ir.AggType(nil), in.AggArgs...)
	}
	for _, d := range in.Defs {
		out.Defs = append(out.Defs, mapRef(d))
	}
	return out
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
