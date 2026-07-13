package opt

import "github.com/evanphx/cg12/ir"

// Pass is one step of the optimization pipeline over a module. Every pass
// reports whether it changed anything, so pipelines (and [Fixpoint]) can iterate
// until the program stops changing.
type Pass interface {
	Name() string
	Run(m *ir.Module) bool
}

// FuncPass lifts a per-function transform to a module pass by applying it to
// every function with a body. Almost every optimization is intraprocedural and
// enters the pipeline this way; only the interprocedural passes ([Inline],
// [DeadFuncElim]) need to see the whole module and are [ModulePass]es.
func FuncPass(name string, run func(*ir.Func) bool) Pass {
	return funcPass{name: name, run: run}
}

type funcPass struct {
	name string
	run  func(*ir.Func) bool
}

func (p funcPass) Name() string { return p.name }

func (p funcPass) Run(m *ir.Module) bool {
	changed := false
	for _, f := range m.Funcs {
		if f.Start == nil {
			continue // a declaration with no body
		}
		if p.run(f) {
			changed = true
		}
	}
	return changed
}

// ModulePass wraps an interprocedural transform that acts on the whole module.
func ModulePass(name string, run func(*ir.Module) bool) Pass {
	return modulePass{name: name, run: run}
}

type modulePass struct {
	name string
	run  func(*ir.Module) bool
}

func (p modulePass) Name() string          { return p.name }
func (p modulePass) Run(m *ir.Module) bool { return p.run(m) }

// Fixpoint runs its member passes in order, repeating the whole sequence until a
// full round changes nothing. Because a Fixpoint is itself a Pass, fixpoints
// nest: the default pipeline wraps "inline then clean" in a fixpoint so that the
// simplification a round of inlining exposes can, in turn, expose the next
// inlining candidate.
func Fixpoint(name string, passes ...Pass) Pass {
	return fixpoint{name: name, passes: passes}
}

type fixpoint struct {
	name   string
	passes []Pass
}

func (fp fixpoint) Name() string { return fp.name }

func (fp fixpoint) Run(m *ir.Module) bool {
	any := false
	for {
		round := false
		for _, p := range fp.passes {
			if p.Run(m) {
				round = true
			}
		}
		if !round {
			return any
		}
		any = true
	}
}

// Run applies a pipeline to a module in order.
func Run(m *ir.Module, pipeline []Pass) {
	for _, p := range pipeline {
		p.Run(m)
	}
}

// DefaultPipeline is the standard optimization pipeline. Its shape encodes the
// interaction between the interprocedural inliner and the intraprocedural
// simplifications: functions are cleaned up before inlining (so cost estimates
// are honest), inlining and cleanup iterate together to a fixpoint (so the
// opportunities inlining exposes are taken, and taking them exposes more), dead
// functions are dropped, and global code motion schedules once at the end.
func DefaultPipeline() []Pass {
	clean := Fixpoint("clean",
		FuncPass("fold", Fold),
		FuncPass("copy", Copy),
		FuncPass("loadelim", LoadElim),
		FuncPass("gvn", GVN),
		FuncPass("simplifycfg", SimplifyCFG),
		FuncPass("dce", DCE),
	)
	return []Pass{
		FuncPass("mem2reg", Mem2Reg),
		clean,
		Fixpoint("inline",
			ModulePass("inline", Inline),
			clean,
		),
		ModulePass("unroll", UnrollRecursion), // bounded in-place recursion unrolling
		Fixpoint("inline", // inline/simplify what unrolling exposed
			ModulePass("inline", Inline),
			clean,
		),
		ModulePass("deadfunc", DeadFuncElim),
		FuncPass("gcm", GCM),
		FuncPass("dce", DCE),
	}
}
