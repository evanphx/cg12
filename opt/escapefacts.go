package opt

import (
	"sort"

	"github.com/evanphx/cg12/ir"
)

// ParamEscape is what a caller needs to know about one parameter of a callee.
//
// The values are ordered as a lattice, worst first: a fixed point that starts
// optimistic and can only lower is a descending chain in this order, so meet is
// min and termination is by construction. See ComputeEscapeFacts.
type ParamEscape uint8

const (
	// ParamEscapes is the assume-the-worst answer: a pointer passed here may be
	// retained past the call by any means.
	ParamEscapes ParamEscape = iota
	// ParamLeaksToResult means the only way the argument outlives the call is
	// through one of the callee's own results, named by ParamFact.Result. A
	// caller that gets this answer must keep asking about that result.
	ParamLeaksToResult
	// ParamNoEscape means nothing the callee does can make the argument outlive
	// the call.
	ParamNoEscape
)

func (escape ParamEscape) String() string {
	switch escape {
	case ParamNoEscape:
		return "noescape"
	case ParamLeaksToResult:
		return "leaks-to-result"
	default:
		return "escapes"
	}
}

// ParamFact is one parameter's summary.
type ParamFact struct {
	Escape ParamEscape
	// Result is the result index a ParamLeaksToResult parameter reaches. It is
	// meaningless for the other two answers.
	Result int
	// Deep is the strong claim: nothing reachable *through* this parameter is
	// retained either, not merely the pointer itself.
	//
	// The three answers above are about the pointer at dereference depth 0,
	// which is the right question when the pointee is a different allocation
	// with an escape decision of its own. It is the wrong question when the
	// pointee is inside the same object -- goc packs a variadic `...any` call's
	// backing array and the payloads its elements point at into one allocation,
	// so a callee that retains an *element* retains the whole object, and a
	// depth-0 ParamNoEscape does not say it does not. See selfReferential in
	// escape.go, which is the only consumer.
	Deep bool
}

// EscapeFacts is a whole program's parameter summary table, keyed by IR symbol
// name. Index 0 is the receiver when the function has one, because on the IR a
// receiver is Params[0] with no marker and "does the receiver escape" and
// "does parameter 0 escape" are the same query.
//
// cg12 compiles whole-program from source, so this is an in-memory map built
// once per compile and thrown away: there is no export data to encode it in,
// nothing to parse, nothing that can go stale, and no ABI to keep compatible.
// That is what lets it carry a leak target rather than a fixed vocabulary of
// tags.
type EscapeFacts struct {
	params map[string][]ParamFact
	Stats  EscapeFactStats
}

// EscapeFactStats records what the solve did, so a caller can report the shape
// of the call graph it ran over rather than asserting it.
type EscapeFactStats struct {
	Funcs          int // module functions
	Bodied         int // functions with a body
	Params         int // parameters summarised, over bodied functions
	NoEscape       int
	LeaksToResult  int
	Escapes        int
	Components     int // strongly connected components of the call graph
	NonTrivialSCCs int // components holding more than one function
	// FuncsInNonTrivialSCCs counts functions in a component of size > 1;
	// SelfRecursiveFuncs counts singleton components that call themselves. Both
	// are cycles the AST walk's `checking` break answers conservatively.
	FuncsInNonTrivialSCCs int
	SelfRecursiveFuncs    int
	LargestSCC            int
	// Rounds is the total number of fixed-point rounds run over recursive
	// components, and Requeries the total number of per-function analyses those
	// rounds cost beyond one each.
	Rounds     int
	Requeries  int
	Bailouts   int // components that hit the round cap and were forced to escape
	Directives int // parameters answered from //go:noescape on a bodiless module function
	// NoEscapeSymbols is how many symbols the front end declared //go:noescape
	// on. Most of them have no ir.Func at all -- they are assembly, or a runtime
	// intrinsic -- so the attribute, not the function, is what carries the fact.
	NoEscapeSymbols int
}

// Param returns the summary for one parameter. A nil table, an unknown symbol
// or an out-of-range index all answer ParamEscapes, so a caller that has no
// facts behaves exactly as one that has not been given any.
func (facts *EscapeFacts) Param(symbol string, index int) ParamFact {
	if facts == nil {
		return ParamFact{}
	}
	params, known := facts.params[symbol]
	if !known || index < 0 || index >= len(params) {
		return ParamFact{}
	}
	return params[index]
}

// Known reports whether the table has an entry for a symbol at all.
func (facts *EscapeFacts) Known(symbol string) bool {
	if facts == nil {
		return false
	}
	_, known := facts.params[symbol]
	return known
}

// Symbols lists every summarised symbol, sorted, so a diff of the table is
// stable.
func (facts *EscapeFacts) Symbols() []string {
	if facts == nil {
		return nil
	}
	symbols := make([]string, 0, len(facts.params))
	for symbol := range facts.params {
		symbols = append(symbols, symbol)
	}
	sort.Strings(symbols)
	return symbols
}

// Params returns the summary of every parameter of one symbol.
func (facts *EscapeFacts) Params(symbol string) []ParamFact {
	if facts == nil {
		return nil
	}
	return facts.params[symbol]
}

// escapeRoundCap bounds the fixed point over one recursive component. The
// lattice is three-valued per parameter and the iteration only lowers, so a
// component of n functions converges in at most a few passes over it; the cap
// exists so that a bug in the transfer function cannot spin, and hitting it
// forces the component to the conservative answer rather than to whatever
// happened to be in the table.
const escapeRoundCap = 64

// ComputeEscapeFacts summarises every function in module.
//
// The solve is bottom-up over the strongly connected components of the direct
// call graph. A component of one non-self-recursive function is answered
// directly: every callee outside it is already final, so the answer cannot
// depend on the order it was asked in. A recursive component starts every
// parameter in it at ParamNoEscape and re-analyses the component until nothing
// changes, which converges to the greatest fixed point -- the correct reading
// of "no use anywhere in this mutually recursive set publishes this".
//
// That is the difference from goc's AST walk, which breaks a cycle by answering
// "escapes" and is therefore both pessimistic and query-order dependent: the
// same question answers true asked standalone and false asked from inside a
// walk that already has the function on its stack.
//
// Optimistic initialisation is a permissive change, so the transfer function
// has to be monotone or the fixed point can settle on "does not escape" for
// something that escapes. It is monotone here by construction: analyzeFunction
// is a pure function of the IR and the current table, and meetFacts clamps each
// round's answer to no higher than the previous one.
func ComputeEscapeFacts(module *ir.Module) *EscapeFacts {
	return computeEscapeFacts(module, false)
}

// ComputeEscapeFactsBreakingCycles is ComputeEscapeFacts with goc's AST walk's
// cycle rule instead of a fixed point: every parameter of every function in a
// recursion cycle is assumed to escape. It exists only to price the fixed
// point, by diffing the two tables.
func ComputeEscapeFactsBreakingCycles(module *ir.Module) *EscapeFacts {
	return computeEscapeFacts(module, true)
}

func computeEscapeFacts(module *ir.Module, breakCycles bool) *EscapeFacts {
	graph := buildCallGraph(module)
	components := computeSCC(graph)

	facts := &EscapeFacts{params: make(map[string][]ParamFact, len(module.Funcs))}
	facts.Stats.Funcs = len(module.Funcs)
	for _, attributes := range module.SymAttrs {
		if attributes.Has(ir.SymNoEscape) {
			facts.Stats.NoEscapeSymbols++
		}
	}
	for _, function := range module.Funcs {
		facts.params[function.Name] = declaredFacts(module, function, &facts.Stats)
	}

	for _, component := range componentsOf(components) {
		facts.Stats.Components++
		recursive := len(component) > 1 || graph.selfRec[component[0]]
		if len(component) > 1 {
			facts.Stats.NonTrivialSCCs++
			facts.Stats.FuncsInNonTrivialSCCs += len(component)
			if len(component) > facts.Stats.LargestSCC {
				facts.Stats.LargestSCC = len(component)
			}
		} else if recursive {
			facts.Stats.SelfRecursiveFuncs++
		}
		if !recursive {
			function := component[0]
			if function.Start != nil {
				facts.params[function.Name] = analyzeFunction(graph.byName, function, facts)
			}
			continue
		}
		if breakCycles {
			// goc's rule: a query that re-enters a function already being
			// walked answers "escapes", so nothing in a cycle is ever proved.
			continue
		}
		solveRecursiveComponent(graph.byName, component, facts)
	}

	for _, function := range module.Funcs {
		if function.Start == nil {
			continue
		}
		facts.Stats.Bodied++
		for _, fact := range facts.params[function.Name] {
			facts.Stats.Params++
			switch fact.Escape {
			case ParamNoEscape:
				facts.Stats.NoEscape++
			case ParamLeaksToResult:
				facts.Stats.LeaksToResult++
			default:
				facts.Stats.Escapes++
			}
		}
	}
	return facts
}

// solveRecursiveComponent runs the optimistic fixed point over one recursive
// component.
func solveRecursiveComponent(byName map[string]*ir.Func, component []*ir.Func, facts *EscapeFacts) {
	for _, function := range component {
		if function.Start == nil {
			continue
		}
		optimistic := make([]ParamFact, len(function.Params))
		for index := range optimistic {
			optimistic[index] = ParamFact{Escape: ParamNoEscape, Deep: true}
		}
		facts.params[function.Name] = optimistic
	}
	for round := 0; ; round++ {
		if round >= escapeRoundCap {
			facts.Stats.Bailouts++
			for _, function := range component {
				if function.Start == nil {
					continue
				}
				facts.params[function.Name] = make([]ParamFact, len(function.Params))
			}
			return
		}
		facts.Stats.Rounds++
		lowered := false
		for _, function := range component {
			if function.Start == nil {
				continue
			}
			if round > 0 {
				facts.Stats.Requeries++
			}
			merged, changed := meetFacts(facts.params[function.Name], analyzeFunction(byName, function, facts))
			if changed {
				facts.params[function.Name] = merged
				lowered = true
			}
		}
		if !lowered {
			return
		}
	}
}

// meetFacts clamps a round's answer to no higher than the previous one, which
// is what makes the iteration a descending chain whatever the transfer function
// does. Two rounds that disagree about which result a parameter leaks to are
// not comparable, so the meet of those is ParamEscapes.
func meetFacts(previous, next []ParamFact) ([]ParamFact, bool) {
	merged := make([]ParamFact, len(previous))
	changed := false
	for index := range previous {
		before := previous[index]
		after := before
		if index < len(next) {
			candidate := next[index]
			switch {
			case candidate.Escape < before.Escape:
				after = candidate
			case candidate.Escape == before.Escape &&
				before.Escape == ParamLeaksToResult && candidate.Result != before.Result:
				after = ParamFact{Escape: ParamEscapes}
			case candidate.Escape == before.Escape:
				// Same answer at depth 0; the deep claim is the conjunction, so
				// one round giving it up gives it up for the component.
				after.Deep = before.Deep && candidate.Deep
			}
		}
		if after != before {
			changed = true
		}
		merged[index] = after
	}
	return merged, changed
}

// declaredFacts is what is known about a function before any body is looked at:
// nothing, unless it has no body and the front end declared //go:noescape on
// it, which promises that no pointer argument is retained past the call.
func declaredFacts(module *ir.Module, function *ir.Func, stats *EscapeFactStats) []ParamFact {
	facts := make([]ParamFact, len(function.Params))
	if function.Start != nil || !module.SymAttrOf(function.Name).Has(ir.SymNoEscape) {
		return facts
	}
	for index := range facts {
		// //go:noescape is the strong claim by construction: whoever wrote the
		// assembly is promising nothing reachable through the argument is kept.
		facts[index] = ParamFact{Escape: ParamNoEscape, Deep: true}
		stats.Directives++
	}
	return facts
}

// componentsOf splits Tarjan's bottom-up order back into components. Tarjan
// appends a component's members contiguously, so a run of equal component ids
// is exactly one component, and the runs come out in reverse topological order:
// every callee outside a component is finalised before the component is solved.
func componentsOf(scc *sccInfo) [][]*ir.Func {
	var components [][]*ir.Func
	for start := 0; start < len(scc.order); {
		end := start + 1
		id := scc.comp[scc.order[start]]
		for end < len(scc.order) && scc.comp[scc.order[end]] == id {
			end++
		}
		components = append(components, scc.order[start:end])
		start = end
	}
	return components
}
