package arm64

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/stackcheck"
)

// The nosplit frame budget. See package stackcheck for the walk; this file is
// where the facts it walks come from on arm64.
//
// The budget runs in the backend because here is the first point at which every
// frame is a number: the layout is done, spills are assigned, and stack objects
// have their sizes. It runs over the module as a whole, after every function has
// been compiled, because the quantity it bounds is a property of a call chain
// and not of any one function.

// The reserve, and how many of it a nosplit chain may spend.
//
// The runtime sets g.stackguard0 = g.stack.lo + stackGuard, where stackGuard is
// stackNosplit + stackSystem + StackSmall (runtime/stack.go). A function with a
// stack-growth check proves at entry that it is above that line, so everything
// below the line is what a run of unchecked frames has to fit inside.
//
// Go's compiler lets a splittable function whose frame is no larger than
// StackSmall compare SP directly against stackguard0 without subtracting its
// frame first, which spends the StackSmall part of the reserve on that frame. Go
// therefore cannot count on it, and cmd/link's doStackCheck uses
// abi.StackNosplitBase alone -- minus 8 on AArch64 for morestack's saved FP,
// which is where its familiar "nosplit stack over 792 byte limit" comes from.
//
// cg12 has no such shortcut. goStackPrologue computes SP-frame and compares that
// against the guard, unconditionally, for every managed frame that is not
// nosplit -- so a cg12 guarded frame really does sit above stack.lo + stackGuard
// and the whole stackGuard, StackSmall included, is available below it.
//
// The extra 128 bytes are not a rounding allowance, they are load-bearing: goc's
// runtime, with no inlining into nosplit callers at all, already has 58 chains
// between 792 and 920 bytes (deepest 856: mProf_Malloc -> ... -> pcvalue). A
// limit of 792 would reject the tree this branch started from.
//
// If goStackPrologue ever grows the small-frame shortcut, stackSmallIsReserved
// must go to false with it.
const (
	stackNosplitBase = 800 // abi.StackNosplitBase
	stackSmall       = 128 // abi.StackSmall
	stackSystem      = 0   // runtime.stackSystem on linux/arm64
	// morestack saves FP below SP on AArch64 before it has a frame of its own.
	framegPointerReserve = 8
	// stackSmallReserved is the StackSmall part of the guard, which cg12's
	// guarded prologue never spends on its own frame. Set it to 0 if
	// goStackPrologue ever grows Go's small-frame shortcut; the limit then
	// becomes Go's own 792.
	stackSmallReserved = stackSmall
)

const noSplitLimit = stackNosplitBase + stackSystem + stackSmallReserved - framegPointerReserve

// noSplitCallSize is what a call instruction pushes before the callee's prologue
// runs. AArch64 is a link-register machine: the return address goes to x30, and
// nothing reaches the stack until the callee saves it inside its own frame,
// which its frame size already covers.
const noSplitCallSize = 0

// stackFacts is one function's contribution to the budget, gathered while the
// function is compiled and carried out of the worker alongside its code.
type stackFacts struct {
	name         string
	frame        int
	guarded      bool // emits a stack-growth check, so it ends a chain
	dynamicAlloc bool
	calls        []string
	indirect     bool
	addressTaken []string
}

// stackFactsFor reads the budget's facts off a function that has just been
// lowered, allocated and emitted.
//
// It reads the *lowered* IR deliberately. Lowering is what turns a source-level
// call into the call that is actually emitted -- it resolves ABI0 entry points,
// materializes runtime helpers, and can replace a call outright -- so the
// pre-lowering call list would describe a program that is not the one being
// built.
//
// "Guarded" is taken from the same condition the prologue is taken from: arm64
// emits goStackPrologue for a managed frame that is not marked NoSplit, and for
// nothing else. A function without a managed frame emits no check either, so it
// does not end a chain; NoSplit on such a function means nothing (the C path
// sets it benignly) and is not consulted.
func stackFactsFor(f *ir.Func, name string, m *mc) stackFacts {
	facts := stackFacts{
		name:         name,
		frame:        m.frame,
		guarded:      f.UsesManagedFrame() && !f.NoSplit,
		dynamicAlloc: m.hasDynAlloc,
	}
	if m.frameless {
		// A frameless leaf never touches SP; its layout frame of 16 is the size
		// the frame record *would* have had. Charging it would inflate every
		// chain that ends in a small accessor, which in runtime code is most of
		// them.
		facts.frame = 0
	}
	seenCall := map[string]bool{}
	seenAddress := map[string]bool{}
	for _, block := range f.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			first := 0
			if instruction.Op == ir.OCall {
				first = 1
				target, direct := directCallSymbol(f, instruction)
				if !direct {
					facts.indirect = true
				} else if !seenCall[target] {
					seenCall[target] = true
					facts.calls = append(facts.calls, target)
				}
			}
			// Any other mention of a symbol is the program getting hold of an
			// address. Whether it ends up in an itab, a closure, a jump table or
			// a global does not matter here: from the budget's point of view it
			// is a value some indirect call could later branch to. Argument 0 of
			// a call is excluded -- that is the direct edge, already counted.
			for _, arg := range instruction.Args[first:] {
				if arg.Kind != ir.RefConst {
					continue
				}
				constant := f.Consts[arg.ID]
				if constant.Kind != ir.ConstSym {
					continue
				}
				target := sanitize(constant.Sym)
				if !seenAddress[target] {
					seenAddress[target] = true
					facts.addressTaken = append(facts.addressTaken, target)
				}
			}
		}
	}
	// A tail call leaves this frame before entering the callee's, so the two do
	// not stack. It is still an edge -- the callee's chain continues from here --
	// and the walk charges this frame for it, which over-counts by one frame on a
	// nosplit tail-call chain. Left as is: over-counting is the safe direction,
	// and buying the accuracy back would mean a second kind of edge in the walk,
	// which is a second thing to get wrong.
	return facts
}

// noSplitBudgetInputs assembles the whole module's facts: the compiled
// functions, the Plan 9 assembly the module carries, and every function address
// the module's data holds.
func noSplitBudgetInputs(compiled []stackFacts, bundle assemblyBundle, data []*ir.Data) []stackcheck.Func {
	assembly := bundle.stack
	funcs := make([]stackcheck.Func, 0, len(compiled)+len(assembly))
	// A function address stored in a global -- an itab's method slot, a jump
	// table, a package-level func variable -- is reachable by an indirect call
	// just as one materialized in a register is. Collecting it here rather than
	// in the per-function pass is what makes an interface method a candidate: its
	// address appears only in the itab.
	addressed := map[string]bool{}
	for _, definition := range data {
		for _, item := range definition.Items {
			if item.Sym != "" {
				addressed[sanitize(item.Sym)] = true
			}
			if item.RelativeTo != "" {
				addressed[sanitize(item.RelativeTo)] = true
			}
		}
	}
	for _, facts := range compiled {
		for _, target := range facts.addressTaken {
			addressed[target] = true
		}
	}
	for target := range bundle.stackAddressTaken {
		addressed[target] = true
	}
	for _, facts := range compiled {
		funcs = append(funcs, stackcheck.Func{
			Name:         facts.name,
			Frame:        facts.frame,
			NoSplit:      !facts.guarded,
			DynamicAlloc: facts.dynamicAlloc,
			Calls:        facts.calls,
			Indirect:     facts.indirect,
			AddressTaken: addressed[facts.name],
		})
	}
	for _, function := range assembly {
		function.AddressTaken = function.AddressTaken || addressed[function.Name]
		funcs = append(funcs, function)
	}
	return funcs
}

// checkNoSplitBudget runs the budget over a finished module.
//
// It is the last thing between a nosplit chain that does not fit and a program
// that dies inside it, so it returns an error and no object is written. There is
// no mode in which it warns: the failure it prevents is a stack overflow in the
// runtime, on some fraction of executions, attributed to whatever code happened
// to be running -- and a build that proceeds with a warning produces exactly
// that, only with a line of text in a log nobody reads.
func checkNoSplitBudget(funcs []stackcheck.Func) error {
	config := stackcheck.Config{
		Limit:          noSplitLimitFromEnvironment(),
		CallSize:       noSplitCallSize,
		StrictIndirect: os.Getenv("GOC_NOSPLIT_INDIRECT") == "strict",
	}
	report, err := stackcheck.Check(funcs, config)
	if debugNoSplit() {
		writeNoSplitReport(os.Stderr, report, config)
	}
	if os.Getenv("GOC_DEBUG_NOSPLIT") == "frames" {
		writeNoSplitFrames(os.Stderr, funcs)
	}
	if err != nil {
		return fmt.Errorf("nosplit frame budget: %w", err)
	}
	return nil
}

// noSplitLimitFromEnvironment is [noSplitLimit], which GOC_NOSPLIT_LIMIT can
// override.
//
// The override exists for the test that has to make the budget fire: a chain
// that overflows 792 bytes on purpose is a large and fragile thing to write and
// keep working, while the same chain against a small limit exercises the same
// code on the same path. It is not a way to make a failing build pass -- raising
// it past the reserve the runtime keeps does not move the reserve.
func noSplitLimitFromEnvironment() int {
	setting := os.Getenv("GOC_NOSPLIT_LIMIT")
	if setting == "" {
		return noSplitLimit
	}
	limit, err := strconv.Atoi(setting)
	if err != nil || limit < 0 {
		return noSplitLimit
	}
	return limit
}

func debugNoSplit() bool {
	return os.Getenv("GOC_DEBUG_NOSPLIT") != ""
}

// writeNoSplitReport prints what the walk found and what it had to assume. The
// assumptions are the point: a budget that reports only a number invites the
// reader to believe it measured more than it did.
func writeNoSplitReport(out io.Writer, report *stackcheck.Report, config stackcheck.Config) {
	if report == nil {
		return
	}
	fmt.Fprintf(out, "goc: nosplit budget: limit %d bytes\n", config.Limit)
	if len(report.Deepest) == 0 {
		fmt.Fprintln(out, "goc: nosplit budget: no nosplit functions")
		return
	}
	fmt.Fprintf(out, "goc: nosplit budget: deepest chain %d bytes\n", report.Height)
	for index, name := range report.Deepest {
		fmt.Fprintf(out, "goc: nosplit budget:   [%d] %s\n", index, name)
	}
	fmt.Fprintf(out, "goc: nosplit budget: %d nosplit callers call indirectly; %d address-taken nosplit functions; %d opaque bodies; %d undefined callees; %d unchecked splittable chain ends\n",
		len(report.IndirectFromNoSplit), len(report.AddressTakenNoSplit), len(report.Opaque), len(report.External), len(report.Unchecked))
	for _, name := range report.Unchecked {
		fmt.Fprintf(out, "goc: nosplit budget: unchecked splittable %s\n", name)
	}
	for _, name := range report.AddressTakenNoSplit {
		fmt.Fprintf(out, "goc: nosplit budget: address-taken nosplit %s\n", name)
	}
	for _, name := range report.Opaque {
		fmt.Fprintf(out, "goc: nosplit budget: opaque nosplit body %s\n", name)
	}
}

// writeNoSplitFrames lists every nosplit frame, largest first. It is how a
// reader answers the question the chain report raises but does not answer: which
// function on this chain is the one to shrink.
func writeNoSplitFrames(out io.Writer, funcs []stackcheck.Func) {
	sorted := append([]stackcheck.Func(nil), funcs...)
	sort.Slice(sorted, func(left, right int) bool {
		if sorted[left].Frame != sorted[right].Frame {
			return sorted[left].Frame > sorted[right].Frame
		}
		return sorted[left].Name < sorted[right].Name
	})
	for _, function := range sorted {
		if !function.NoSplit || function.Frame == 0 {
			continue
		}
		fmt.Fprintf(out, "goc: nosplit frame: %8d  %s\n", function.Frame, function.Name)
	}
}
