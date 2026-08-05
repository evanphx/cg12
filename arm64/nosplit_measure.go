package arm64

import (
	"sort"

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

	// sharing is who lies on a chain with whom, and charged is what has already
	// been spent on each function's chains. Together they are what keeps the
	// headroom map from being spent twice; see Charge.
	sharing map[string][]string
	charged map[string]int
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
	inputs := noSplitBudgetInputs(facts, bundle, m.Data)
	report, _ := stackcheck.Check(inputs, stackcheck.Config{
		Limit:    noSplitLimitFromEnvironment(),
		CallSize: noSplitCallSize,
	})
	if report == nil {
		return nil, nil
	}
	return &noSplitFrameBudget{
		headroom:    report.Headroom,
		conventions: conventions,
		bundle:      bundle,
		sharing:     noSplitChainSharing(inputs),
		charged:     map[string]int{},
	}, nil
}

func (b *noSplitFrameBudget) Headroom(name string) int {
	room, ok := b.headroom[name]
	if !ok {
		// A function the walk did not see is a function whose chains were not
		// measured. Offer it nothing.
		return 0
	}
	return room - b.charged[name]
}

// Charge takes bytes out of the headroom of name and of everything that shares a
// nosplit chain with it.
//
// Two functions share a chain exactly when one can reach the other without a
// stack check in between, so the set is name plus its nosplit descendants plus
// its nosplit ancestors. Charging all three is what makes a sequence of
// independent per-function decisions add up to a bound on the chain: whichever
// function is asked next, the slack it is offered has every earlier grant on any
// chain they have in common already subtracted.
func (b *noSplitFrameBudget) Charge(name string, bytes int) {
	if bytes <= 0 {
		return
	}
	for _, shared := range b.sharing[name] {
		b.charged[shared] += bytes
	}
}

// noSplitChainSharing maps every nosplit function to the nosplit functions that
// lie on a chain with it, itself included.
//
// The edges are the ones the walk itself follows (stackcheck.walker.calleesOf):
// a splittable callee ends the chain and is not an edge, and an indirect call is
// assumed to reach a function that checks its own stack, which is the same
// default the budget is measured under. A relation built from any other edge set
// would be charging against chains the check does not believe in.
func noSplitChainSharing(funcs []stackcheck.Func) map[string][]string {
	noSplit := make(map[string]bool, len(funcs))
	for index := range funcs {
		if funcs[index].NoSplit {
			noSplit[funcs[index].Name] = true
		}
	}
	callees := map[string][]string{}
	callers := map[string][]string{}
	for index := range funcs {
		f := &funcs[index]
		if !f.NoSplit {
			continue
		}
		for _, callee := range f.Calls {
			if !noSplit[callee] || callee == f.Name {
				continue
			}
			callees[f.Name] = append(callees[f.Name], callee)
			callers[callee] = append(callers[callee], f.Name)
		}
	}
	sharing := make(map[string][]string, len(noSplit))
	for name := range noSplit {
		reached := map[string]bool{name: true}
		reach(name, callees, reached)
		reach(name, callers, reached)
		shared := make([]string, 0, len(reached))
		for other := range reached {
			shared = append(shared, other)
		}
		sort.Strings(shared)
		sharing[name] = shared
	}
	return sharing
}

// reach adds everything edges lead to from origin into visited. The origin is
// already in visited, so a cycle back to it terminates rather than recursing.
func reach(origin string, edges map[string][]string, visited map[string]bool) {
	stack := []string{origin}
	for len(stack) > 0 {
		name := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, next := range edges[name] {
			if visited[next] {
				continue
			}
			visited[next] = true
			stack = append(stack, next)
		}
	}
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
