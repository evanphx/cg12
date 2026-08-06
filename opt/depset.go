package opt

import (
	"sort"
	"time"

	"github.com/evanphx/cg12/ir"
)

// InlineDeps is the dynamic dependency file BUILD_CACHE.md's Option C is keyed
// on: for each function, the set of other module functions the interprocedural
// passes read while turning that function's post-`clean` body into the body the
// backend compiles.
//
// The point of recording it is that Option C's whole ceiling rests on a claim
// the blast-radius measurement does not make. §2.4 shows the OUTPUT is stable --
// 4130 of 4131 post-optimisation functions are byte-identical after a root-package
// edit. A memoiser cannot use that. It has to decide, before doing the work,
// whether the work it did last time is still valid, and the only thing it has to
// decide with is a recorded set. If that set is much larger than the set of
// functions the edit actually perturbed, the memo misses anyway and the stable
// output buys nothing.
//
// Two sets are recorded, and they bracket the answer:
//
//   - Consulted is what soundness requires. A function lands in it when the
//     inliner resolved it by name on this caller's behalf -- which is to say, the
//     inliner read its size, its attributes, its structure, and its position in
//     the call graph, and *some* decision about the caller came out of that read.
//     Changing the callee could change the decision, whether or not the callee
//     was ever spliced. This is the set a real memoiser must validate.
//   - Spliced is the optimistic floor: only the functions whose bodies actually
//     ended up inside this one. It is not a sound key on its own (a callee that
//     grew past the budget changes a caller that never inlined it, and a callee
//     that shrank below it changes a caller that never inlined it either), but it
//     bounds how much precision a smarter key could ever recover.
//
// Both sets are transitively closed through splicing: when g's body is cloned
// into f, everything that determined g's body also determines f's, so f inherits
// g's sets along with g itself.
//
// The recorder is off unless [Record] turns it on, and while it is off the hooks
// are a nil pointer comparison on a path that already does a map lookup.
type InlineDeps struct {
	consulted map[*ir.Func]map[*ir.Func]bool
	spliced   map[*ir.Func]map[*ir.Func]bool
	// noSplitMeasured names the functions InlineIntoNoSplitCallers laid a frame
	// out for. They carry a dependency this file cannot express -- see
	// [InlineDeps.NoSplitMeasured].
	noSplitMeasured map[*ir.Func]bool
	// noSplitAccepted names the functions whose measured frame fit, so the pass
	// kept the inlining and charged the chain.
	noSplitAccepted map[*ir.Func]bool
	// unrolled names the functions UnrollRecursion spliced a back-edge into.
	unrolled map[*ir.Func]bool
	// costInlineSelected names the functions selectCostInline marked, a
	// whole-module decision made once on the original call graph.
	costInlineSelected map[*ir.Func]bool
	// siteCounts is the module-wide direct-call-site count per function, as the
	// first inlining round computed it. It is a whole-module quantity feeding a
	// per-function decision, so it has to be reported separately from the sets.
	siteCounts map[*ir.Func]int
	// graphTime and graphBuilds account the whole-module analyses the inliner
	// redoes on every round -- the call graph, its SCC condensation and the
	// call-site census. They are the standing cost of Option C: no per-function
	// memo can skip them, because they are what says which functions the memo is
	// even allowed to be keyed on.
	graphTime   time.Duration
	graphBuilds int
}

// activeDeps is the recorder the hooks below write to, or nil.
//
// It is process-global because the alternative is threading a recorder through
// buildCallGraph, callSiteCounts, the four findInlinable-shaped scans, the two
// unroller entry points and spliceCall, none of which otherwise know anything
// about caching. Measurement scaffolding does not get to reshape the inliner's
// signatures. [Record] is the only thing that writes it, it panics rather than
// nest, and it restores nil on the way out, so a compile that is not being
// recorded runs exactly as before.
var activeDeps *InlineDeps

// Record runs compile with the dependency recorder on and returns what it saw.
// It is not safe to run two recorded compiles at once, and it says so rather
// than quietly interleaving them.
func Record(compile func()) *InlineDeps {
	if activeDeps != nil {
		panic("opt: opt.Record is already recording; dependency recording does not nest")
	}
	deps := &InlineDeps{
		consulted:          map[*ir.Func]map[*ir.Func]bool{},
		spliced:            map[*ir.Func]map[*ir.Func]bool{},
		noSplitMeasured:    map[*ir.Func]bool{},
		noSplitAccepted:    map[*ir.Func]bool{},
		unrolled:           map[*ir.Func]bool{},
		costInlineSelected: map[*ir.Func]bool{},
		siteCounts:         map[*ir.Func]int{},
	}
	activeDeps = deps
	defer func() { activeDeps = nil }()
	compile()
	return deps
}

// noteConsulted records that the inliner resolved callee by name while working
// on caller.
func (d *InlineDeps) noteConsulted(caller, callee *ir.Func) {
	if caller == nil || callee == nil || caller == callee {
		return
	}
	set := d.consulted[caller]
	if set == nil {
		set = map[*ir.Func]bool{}
		d.consulted[caller] = set
	}
	set[callee] = true
}

// noteSpliced records that callee's body was cloned into caller, and moves
// callee's own dependencies across: the caller now contains a copy of the code
// those dependencies produced.
func (d *InlineDeps) noteSpliced(caller, callee *ir.Func) {
	if caller == nil || callee == nil {
		return
	}
	d.noteConsulted(caller, callee)
	for dep := range d.consulted[callee] {
		d.noteConsulted(caller, dep)
	}
	if caller == callee {
		return // self-recursion unrolling: the body is already its own
	}
	set := d.spliced[caller]
	if set == nil {
		set = map[*ir.Func]bool{}
		d.spliced[caller] = set
	}
	set[callee] = true
	for dep := range d.spliced[callee] {
		set[dep] = true
	}
}

// Consulted returns the sound dependency set of f, sorted by name.
func (d *InlineDeps) Consulted(f *ir.Func) []*ir.Func { return sorted(d.consulted[f]) }

// Spliced returns the functions whose bodies ended up inside f, sorted by name.
func (d *InlineDeps) Spliced(f *ir.Func) []*ir.Func { return sorted(d.spliced[f]) }

// NoSplitMeasured returns the functions InlineIntoNoSplitCallers laid a frame
// out for, sorted by name.
//
// They are called out because that pass is the one place in the pipeline where a
// per-function dependency set is not the whole story. Its input is a byte count
// the backend computes from the finished code, and its FrameBudget.Charge spends
// a chain's headroom in call-graph order -- so one accepted caller can change
// what a later caller on the same nosplit chain is allowed to do. That is a
// dependency on an ordering, not on a set of functions, and no per-function key
// expresses it.
func (d *InlineDeps) NoSplitMeasured() []*ir.Func { return sorted(d.noSplitMeasured) }

// NoSplitAccepted returns the functions whose measured frame fit, so the pass
// kept what it inlined and charged the chain.
func (d *InlineDeps) NoSplitAccepted() []*ir.Func { return sorted(d.noSplitAccepted) }

// Unrolled returns the functions UnrollRecursion spliced a back-edge into.
func (d *InlineDeps) Unrolled() []*ir.Func { return sorted(d.unrolled) }

// CostInlineSelected returns the functions selectCostInline marked.
func (d *InlineDeps) CostInlineSelected() []*ir.Func { return sorted(d.costInlineSelected) }

// SiteCount returns the module-wide direct-call-site count the first inlining
// round computed for f.
func (d *InlineDeps) SiteCount(f *ir.Func) int { return d.siteCounts[f] }

// SiteCounts returns the whole recorded call-site table.
func (d *InlineDeps) SiteCounts() map[*ir.Func]int { return d.siteCounts }

// GraphCost returns the wall time the inliner spent rebuilding the call graph,
// its SCC condensation and the call-site census, and how many times it did so.
func (d *InlineDeps) GraphCost() (time.Duration, int) { return d.graphTime, d.graphBuilds }

// noteGraph accounts one rebuild of the whole-module analyses.
func (d *InlineDeps) noteGraph(elapsed time.Duration) {
	d.graphTime += elapsed
	d.graphBuilds++
}

func sorted(set map[*ir.Func]bool) []*ir.Func {
	out := make([]*ir.Func, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
