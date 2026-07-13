package opt

import "github.com/evanphx/cg12/ir"

// inlineInstrBudget bounds the size (in instructions) of a callee that will be
// inlined. Small functions are the ones where inlining pays: the call overhead
// is a large fraction of the body, and the caller's context (constant
// arguments, known aliasing) folds through what remains.
const inlineInstrBudget = 24

// inlineFuncBudget bounds how many calls are inlined into one function per pass,
// a backstop against code-size blow-up; the pipeline's fixpoint applies more on
// later rounds if they remain worthwhile.
const inlineFuncBudget = 64

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
	cg := buildCallGraph(m)
	scc := computeSCC(cg)
	changed := false
	for _, f := range scc.order {
		if f.Start == nil {
			continue
		}
		if inlineInto(f, cg, scc) {
			changed = true
		}
	}
	return changed
}

// inlineInto repeatedly inlines eligible calls in caller until none remain or
// the per-function budget is spent. Re-scanning after each splice lets calls
// exposed by a previous inline (the callee's own calls) be inlined in turn; the
// budget bounds code-size growth (e.g. a diamond call graph inlined repeatedly).
func inlineInto(caller *ir.Func, cg *callGraph, scc *sccInfo) bool {
	changed := false
	for n := 0; n < inlineFuncBudget; n++ {
		b, idx, callee := findInlinable(caller, cg, scc)
		if callee == nil {
			break
		}
		spliceCall(caller, b, idx, callee, cg, scc, 0)
		changed = true
	}
	return changed
}

// findInlinable returns the first call in caller that should be inlined: a
// direct call to a non-recursive, suitably small module function.
func findInlinable(caller *ir.Func, cg *callGraph, scc *sccInfo) (*ir.Block, int, *ir.Func) {
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
			if inlinable(callee) {
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

// inlinable reports whether callee is a small, self-contained scalar function
// suitable for inlining. Recursion is not checked here — it is a call-graph
// property handled by the SCC classification in findInlinable.
func inlinable(callee *ir.Func) bool {
	if callee.Start == nil || callee.Variadic || callee.RetAgg != nil {
		return false
	}
	for _, p := range callee.Params {
		if p.Agg != nil {
			return false // by-value aggregate parameter
		}
	}
	instrs, hasRet := 0, false
	for _, b := range callee.Blocks {
		instrs += len(b.Instrs)
		if b.Jmp.Kind == ir.JmpRet {
			hasRet = true
		}
	}
	return hasRet && instrs <= inlineInstrBudget
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
			cl := cloneInstr(&s.instrs[i], mapRef)
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
					in.Aux = int64(depth + 1)
				}
			}
		}
	}
}

// cloneInstr copies an instruction, remapping every value reference.
func cloneInstr(in *ir.Instr, mapRef func(ir.Ref) ir.Ref) ir.Instr {
	out := ir.Instr{Op: in.Op, Cls: in.Cls, To: mapRef(in.To), Cmp: in.Cmp, Aux: in.Aux, RetAgg: in.RetAgg, Pos: in.Pos}
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
		return f.SymClass(c.Sym, c.Int, c.Cls)
	}
	return ir.R
}
