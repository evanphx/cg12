package opt

import "github.com/evanphx/cg12/ir"

// RecursiveFunctions reports, per module function, whether it participates in a
// recursion cycle -- the `scc.recursive` bit the inliner gates every inlining
// decision on.
//
// It is exported for the Option C memo, which needs it for a reason stage 1
// stated precisely: SCC membership is a property of the whole call graph and not
// of any function a recorded dependency set names, so it is the one input a
// per-function key cannot express. The failure is constructible -- f inlines
// small g; g calls large x that g did not inline, so x's callees never entered
// f's set; an edit makes a callee of x call back into g's component, g becomes
// recursive, and f may no longer inline it, with nothing in f's recorded set
// having moved. Recording each set member's bit and revalidating it against a
// rebuilt condensation closes that, and the rebuild is not an extra cost: the
// residue passes a memoised compile still has to run need the graph anyway.
//
// The classification is of the module as it stands, so a memoised compile takes
// it at the same point the reference compile's first inlining round does: over
// the post-prefix bodies, before anything is spliced.
func RecursiveFunctions(m *ir.Module) map[*ir.Func]bool {
	scc := computeSCC(buildCallGraph(m))
	recursive := make(map[*ir.Func]bool, len(scc.recursive))
	for f, yes := range scc.recursive {
		if yes {
			recursive[f] = true
		}
	}
	return recursive
}

// CostInlineSelected names the functions [selectCostInline] would mark on this
// module -- the one live whole-module input to an inlining decision that is
// neither a dependency-set member nor an SCC bit.
//
// It is 0 functions on both Go programs measured (it fires only on callers
// holding a computed goto, which is an interpreter's threaded dispatch loop), so
// on Go it is inert; it is live for the C interpreter workloads. A memo key
// covers it with a digest of this set rather than by pretending it does not
// exist.
func CostInlineSelected(m *ir.Module) []string {
	cg := buildCallGraph(m)
	scc := computeSCC(cg)
	before := make(map[*ir.Func]bool, len(m.Funcs))
	for _, f := range m.Funcs {
		before[f] = f.CostInline
	}
	selectCostInline(m, cg, scc)
	var selected []string
	for _, f := range m.Funcs {
		if f.CostInline && !before[f] {
			selected = append(selected, f.Name)
		}
		f.CostInline = before[f] // asking must not be the same as doing
	}
	return selected
}
