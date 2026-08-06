package opt

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/evanphx/cg12/ir"
)

// FrameBudget is a backend's account of the stack a nosplit function may still
// spend. It is what lets an IR pass be bounded by a quantity IR does not
// contain.
//
// The inliner works on instructions and cannot see a frame; the backend lays out
// frames and cannot inline. Every previous attempt to reconcile the two guessed:
// either the inliner guessed at frame growth from a code-size proxy, or it
// refused to touch nosplit functions at all. This interface is the third option
// -- the inliner asks the backend, in bytes.
type FrameBudget interface {
	// Headroom is how many bytes name's frame may grow before the deepest
	// nosplit chain running through it reaches the limit. It is zero or negative
	// for a function with nothing to spend, and for one that is over already.
	Headroom(name string) int

	// Charge records that name's frame has grown by bytes and takes those bytes
	// out of the headroom of every function that shares a nosplit chain with it.
	//
	// A budget is measured once and then spent many times, which is the one way
	// the arithmetic above can be wrong. Headroom is a property of a chain, not
	// of a function: two nosplit functions on the same chain are each told the
	// chain's whole remaining slack, and a pass that believes both of them can
	// spend it twice. Charging closes that -- after the first caller grows, every
	// other function on its chains is offered what is left rather than what there
	// was.
	Charge(name string, bytes int)

	// Frame lays f out as the backend would and returns its frame size in bytes.
	// It must not disturb f.
	Frame(f *ir.Func) (int, error)

	// Symbol is the linker name the backend knows f by, which is the name
	// Headroom is keyed on.
	Symbol(f *ir.Func) string
}

// NoSplitFrameBudgetFor produces the frame budget for a module. A backend
// registers it; the pass below does nothing when no backend has.
//
// It is a registration hook rather than an argument because the pass has to sit
// inside [DefaultPipeline] to be worth anything -- a nosplit function that only
// gets inlined into when some particular driver remembers to ask for it is a
// nosplit function that is optimized differently depending on who compiled it --
// and the pipeline is assembled in this package, which cannot import a backend.
var NoSplitFrameBudgetFor func(*ir.Module) (FrameBudget, error)

// noSplitInlineSafetyBytes is held back from every caller's headroom.
//
// The pass measures the frame it produced, so the byte count it checks is real,
// not estimated -- but it measures the caller alone. A frame is also affected by
// what the *rest* of the pipeline does afterwards, and by anything downstream
// that can add a spill slot. Sixteen bytes is one AArch64 stack slot pair: enough
// that a frame is not accepted at exactly the limit, small enough that it does
// not swallow a caller's whole allowance.
const noSplitInlineSafetyBytes = 16

// InlineIntoNoSplitCallers inlines into functions that emit no stack-growth
// check, bounded by how much stack their chains have left.
//
// This is the pass the stopgap in [frameIsSpentFromTheNoSplitReserve] stands in
// for. That rule -- never inline into a nosplit caller -- was the only one
// statable without a frame model, and it cost the runtime's allocator fast paths
// their inlining, which is where inlining is worth most. With a frame model
// there is a better rule, and it is this one: inline, measure what the frame
// actually became, and keep the result only if the chain still fits.
//
// The measurement is the whole point. Nothing here estimates. For each caller
// the pass takes a snapshot, inlines, cleans up the result so the measurement
// sees the code the backend will see, lays the frame out through the backend,
// and compares. A caller whose frame grew past its headroom is restored from the
// snapshot exactly as it was; there is no partial credit and no rounding in
// anyone's favour.
//
// What it does not do is give a caller more room than the budget says it has. A
// nosplit chain that is already over the reserve has negative headroom and gets
// nothing, which is the right answer even though it means the case that
// motivated all of this -- runtime.mcache.nextFree, whose chain is 1104 bytes
// against a 920-byte reserve before anything is inlined -- is refused. Inlining
// into it was never affordable; the stopgap was hiding a chain that was already
// too deep, not creating one.
func InlineIntoNoSplitCallers(m *ir.Module) bool {
	_, changed := InlineIntoNoSplitCallersReporting(m)
	return changed
}

// NoSplitInlineReport describes what [InlineIntoNoSplitCallers] did, for the
// GOC_DEBUG_NOSPLIT dump and for tests. It is filled in only when a caller is
// watching.
type NoSplitInlineReport struct {
	Accepted []NoSplitInlineResult
	Rejected []NoSplitInlineResult
	NoRoom   []string
}

// NoSplitInlineResult is one caller's outcome.
type NoSplitInlineResult struct {
	Name      string
	Allowance int
	Before    int
	After     int
}

// InlineIntoNoSplitCallersReporting is [InlineIntoNoSplitCallers] with the
// per-caller outcomes recorded. The pass is worth measuring -- the question it
// exists to answer is what lifting the restriction bought -- and a pass that
// can only be measured by diffing object files is a pass nobody measures.
func InlineIntoNoSplitCallersReporting(m *ir.Module) (*NoSplitInlineReport, bool) {
	report := &NoSplitInlineReport{}
	if NoSplitFrameBudgetFor == nil || os.Getenv("GOC_NO_NOSPLIT_INLINE") != "" {
		return report, false
	}
	budget, err := NoSplitFrameBudgetFor(m)
	if err != nil || budget == nil {
		return report, false
	}
	graph := buildCallGraph(m)
	components := computeSCC(graph)
	sites := callSiteCounts(m, graph.byName)
	base := map[*ir.Func]int{}
	changed := false
	for _, caller := range components.order {
		if caller.Start == nil || !caller.NoSplit || hasSecondaryEntry(caller) {
			continue
		}
		name := budget.Symbol(caller)
		allowance := budget.Headroom(name) - noSplitInlineSafetyBytes
		if allowance <= 0 {
			report.NoRoom = append(report.NoRoom, name)
			continue
		}
		snapshot, err := ir.CloneFunc(caller)
		if err != nil {
			continue
		}
		before, err := budget.Frame(caller)
		if err != nil {
			continue
		}
		if activeDeps != nil {
			activeDeps.noSplitMeasured[caller] = true
		}
		if !inlineInto(caller, graph, components, sites, base) {
			continue
		}
		Optimize(caller)
		after, err := budget.Frame(caller)
		result := NoSplitInlineResult{Name: name, Allowance: allowance, Before: before, After: after}
		if err != nil || after-before > allowance {
			caller.ReplaceBodyFrom(snapshot)
			report.Rejected = append(report.Rejected, result)
			continue
		}
		if after > before {
			// Take the growth out of every chain this caller sits on, so the next
			// caller on the same chain is offered what is left. A caller that
			// shrank -- most of them do, because inlining removes the outgoing
			// argument area -- is not credited back: the headroom map was measured
			// before any of this, and handing out bytes it never counted would be
			// the same mistake in the other direction.
			budget.Charge(name, after-before)
		}
		report.Accepted = append(report.Accepted, result)
		if activeDeps != nil {
			activeDeps.noSplitAccepted[caller] = true
		}
		changed = true
	}
	sort.Strings(report.NoRoom)
	sort.Slice(report.Accepted, func(left, right int) bool {
		return report.Accepted[left].Name < report.Accepted[right].Name
	})
	sort.Slice(report.Rejected, func(left, right int) bool {
		return report.Rejected[left].Name < report.Rejected[right].Name
	})
	if os.Getenv("GOC_DEBUG_NOSPLIT") == "inline" {
		report.write(os.Stderr)
	}
	return report, changed
}

// write prints what the pass did. The two interesting populations are the
// callers it accepted -- with the frame growth it measured, so the number can be
// checked -- and the callers it had no room for at all, which is the standing
// cost of the runtime's nosplit chains being as deep as they are.
func (r *NoSplitInlineReport) write(out io.Writer) {
	fmt.Fprintf(out, "goc: nosplit inline: %d accepted, %d rejected after measuring, %d with no headroom\n",
		len(r.Accepted), len(r.Rejected), len(r.NoRoom))
	for _, result := range r.Accepted {
		fmt.Fprintf(out, "goc: nosplit inline: accepted %s frame %d -> %d (+%d of %d allowed)\n",
			result.Name, result.Before, result.After, result.After-result.Before, result.Allowance)
	}
	for _, result := range r.Rejected {
		fmt.Fprintf(out, "goc: nosplit inline: reverted %s frame %d -> %d (+%d, %d allowed)\n",
			result.Name, result.Before, result.After, result.After-result.Before, result.Allowance)
	}
	for _, name := range r.NoRoom {
		fmt.Fprintf(out, "goc: nosplit inline: no headroom %s\n", name)
	}
}
