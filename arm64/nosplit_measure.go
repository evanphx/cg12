package arm64

import (
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/stackcheck"
)

// This is the backend half of "bounded by the budget": the frame numbers an IR
// pass cannot compute for itself, offered through opt.FrameBudget.
//
// Registering it here rather than having the pipeline import a backend keeps the
// dependency pointing the way it already does -- a backend knows about the
// optimizer, the optimizer does not know about backends -- and means every
// driver that links this backend gets the same answer, so a nosplit function is
// not optimized differently depending on who compiled it.
func init() {
	opt.NoSplitFrameBudgetFor = func(m *ir.Module) (opt.FrameBudget, error) {
		return newNoSplitFrameBudget(m)
	}
}

type noSplitFrameBudget struct {
	headroom    map[string]int
	conventions calleeConventions
	bundle      assemblyBundle
}

// newNoSplitFrameBudget measures every nosplit function in m, walks the chains
// they form, and returns how much room each has left.
//
// Only the nosplit functions are measured. A splittable function's frame does
// not enter any chain -- it proves its own frame fits and the chain restarts
// below it -- so it goes into the graph as a name with no frame, which is all
// the walk needs to know that a chain ends there. That is what keeps this
// affordable: a Go runtime module has on the order of 600 nosplit functions out
// of 15000, so the measurement is a few percent of a compile rather than a
// second one.
//
// It returns a nil budget, and no error, for a module the budget does not apply
// to: one that does not use the Go runtime has no stack-growth check for a
// function to be exempt from, so it has no nosplit chains in the sense meant
// here.
func newNoSplitFrameBudget(m *ir.Module) (opt.FrameBudget, error) {
	if !moduleUsesGoRuntime(m) {
		return nil, nil
	}
	bundle, err := prepareAssembly(m)
	if err != nil {
		return nil, err
	}
	functions := make([]*ir.Func, 0, len(m.Funcs)+len(bundle.lowered))
	functions = append(functions, m.Funcs...)
	functions = append(functions, bundle.lowered...)
	conventions := newCalleeConventions(functions)

	facts := make([]stackFacts, 0, len(functions))
	for _, f := range functions {
		if f == nil || f.Start == nil {
			continue
		}
		name := sanitize(f.Name)
		if !f.NoSplit || !f.UsesManagedFrame() {
			facts = append(facts, stackFacts{name: name, guarded: f.UsesManagedFrame()})
			continue
		}
		measured, err := measureFunction(f, name, conventions, bundle)
		if err != nil {
			// A function the backend cannot lay out yet is not a reason to give up
			// on the module: it will be reported when the module is really
			// compiled. Leaving it out understates the headroom of everything that
			// calls it, which is the safe direction.
			continue
		}
		facts = append(facts, measured)
	}
	report, _ := stackcheck.Check(noSplitBudgetInputs(facts, bundle, m.Data), stackcheck.Config{
		Limit:    noSplitLimitFromEnvironment(),
		CallSize: noSplitCallSize,
	})
	if report == nil {
		return nil, nil
	}
	return &noSplitFrameBudget{headroom: report.Headroom, conventions: conventions, bundle: bundle}, nil
}

func (b *noSplitFrameBudget) Headroom(name string) int {
	room, ok := b.headroom[name]
	if !ok {
		// A function the walk did not see is a function whose chains were not
		// measured. Offer it nothing.
		return 0
	}
	return room
}

func (b *noSplitFrameBudget) Symbol(f *ir.Func) string {
	return sanitize(f.Name)
}

func (b *noSplitFrameBudget) Frame(f *ir.Func) (int, error) {
	measured, err := measureFunction(f, sanitize(f.Name), b.conventions, b.bundle)
	if err != nil {
		return 0, err
	}
	return measured.frame, nil
}

// measureFunction lays f out exactly as the emit loop would, and stops there.
//
// It works on a clone: lowering and register allocation rewrite a function into
// something only the emitter can read, so measuring the real one would consume
// it -- and the point of measuring is to decide what to do with it next.
//
// It stops at the layout rather than going on to emit code, because the frame is
// finished at that point and the machine code is the expensive part. The layout
// is computed by the same computeFrame the emitter calls, from the same
// allocation, so it is not an approximation of the emitter's answer; it is the
// emitter's answer.
func measureFunction(f *ir.Func, name string, conventions calleeConventions, bundle assemblyBundle) (stackFacts, error) {
	clone, err := ir.CloneFunc(f)
	if err != nil {
		return stackFacts{}, err
	}
	applyAssemblyCallConventions(clone, bundle.callConventions)
	if clone.UsesManagedFrame() {
		prepareGoABI(clone)
	}
	ir.LowerPointers(clone, ptrCls)
	if err := lower(clone, conventions, TLSModel(0)); err != nil {
		return stackFacts{}, err
	}
	alloc, err := regAlloc(clone)
	if err != nil {
		return stackFacts{}, err
	}
	layout := computeFrame(clone, alloc, conventions)
	return stackFactsFor(clone, name, layout, framelessEligible(clone, layout, nil)), nil
}
