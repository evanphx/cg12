package opt

import "github.com/evanphx/cg12/ir"

// maxRecursionDepth is how many levels a recursion cycle is unrolled in place.
// A recursive call is inlined this many times; the deepest one is left as a real
// call, so the program still terminates by ordinary recursion.
const maxRecursionDepth = 2

// UnrollRecursion unrolls recursion cycles in place, like loop unrolling: within
// a strongly-connected component of the call graph, a back-edge call is inlined
// up to maxRecursionDepth levels deep. Only intra-SCC calls are unrolled, so
// code growth stays inside the recursive functions themselves — a call that
// merely enters the cycle from outside is untouched.
//
// It runs once, not in a fixpoint: the depth reached along each cloned recursive
// call is tracked transiently in the call's Aux slot and cleared before
// returning, so nothing about inlining leaks past the pass.
func UnrollRecursion(m *ir.Module) bool {
	cg := buildCallGraph(m)
	scc := computeSCC(cg)
	changed := false
	for _, f := range scc.order {
		if f.Start == nil {
			continue
		}
		if unrollInto(f, cg, scc) {
			changed = true
		}
	}
	if changed {
		for _, f := range m.Funcs {
			clearCallDepth(f)
		}
	}
	return changed
}

// unrollInto unrolls caller's intra-SCC recursive calls until each cloned chain
// reaches maxRecursionDepth or the per-function budget is spent.
func unrollInto(caller *ir.Func, cg *callGraph, scc *sccInfo) bool {
	changed := false
	for n := 0; n < inlineFuncBudget; n++ {
		b, idx, callee := findUnrollable(caller, cg, scc)
		if callee == nil {
			break
		}
		depth := int(b.Instrs[idx].Aux)
		spliceCall(caller, b, idx, callee, cg, scc, depth)
		changed = true
	}
	return changed
}

// findUnrollable returns the first recursion back-edge in caller that is still
// below the depth limit: a direct call whose callee is in caller's own SCC and
// whose recorded depth has not reached maxRecursionDepth.
func findUnrollable(caller *ir.Func, cg *callGraph, scc *sccInfo) (*ir.Block, int, *ir.Func) {
	for _, b := range caller.Blocks {
		for i := range b.Instrs {
			in := &b.Instrs[i]
			if in.Op != ir.OCall {
				continue
			}
			callee := directCallee(caller, in, cg.byName)
			if callee == nil || scc.comp[callee] != scc.comp[caller] {
				continue // not a back-edge inside this recursion cycle
			}
			if int(in.Aux) >= maxRecursionDepth {
				continue // this chain is already unrolled to the limit
			}
			if inlinable(callee) {
				return b, i, callee
			}
		}
	}
	return nil, 0, nil
}

// clearCallDepth removes the transient depth marks left in call Aux slots.
func clearCallDepth(f *ir.Func) {
	for _, b := range f.Blocks {
		for i := range b.Instrs {
			if b.Instrs[i].Op == ir.OCall {
				b.Instrs[i].Aux = 0
			}
		}
	}
}
