package opt

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
)

// The consumer side of the fact table: what LowerHeapAllocations does with a
// summary once it has one.
//
// The pass's shape is unchanged. Without a table it reaches a call it does not
// recognise, cannot know what the callee does with the pointer, and escapes
// every argument -- which is why it promotes 3.1% of the candidates it sees,
// since most allocations reach a call. With a table it asks, per argument, and
// only escapes the ones the callee can actually retain.

// markSummarisedCall escapes exactly those arguments of a direct call that the
// callee's summary says it may retain.
//
// The callee reference itself is marked unconditionally: for a direct call it
// is a constant and marking is a no-op, and for an indirect call it is the
// function value, which the no-summary arm escapes too.
func markSummarisedCall(
	function *ir.Func,
	byName map[string]*ir.Func,
	facts *EscapeFacts,
	instruction ir.Instr,
	mark func(ir.Ref, string),
) {
	mark(instruction.Arg(0), "callee of an indirect call")
	callee := constSymbolName(function, instruction.Arg(0))
	if calleeRetainsNothing(function, callee) {
		return
	}
	target, arguments, usable := summarisedCallee(byName, callee, instruction)
	if !usable {
		why := summaryUnavailableReason(callee, target, instruction)
		for _, argument := range arguments {
			mark(argument, why)
		}
		return
	}
	for index, argument := range arguments {
		fact := facts.Param(callee, index)
		switch fact.Escape {
		case ParamNoEscape:
			// The callee cannot make the argument outlive the call.
		case ParamLeaksToResult:
			if callLeaksToTrackedResult(instruction, target, fact) {
				// The call's result carries it; the analysis already tracked the
				// result as a derivation of the same allocation, so whether it
				// escapes is decided where the result is used.
				continue
			}
			mark(argument, fmt.Sprintf("argument %d of $%s leaks to a result the caller cannot follow", index, callee))
		default:
			mark(argument, fmt.Sprintf("argument %d of $%s escapes", index, callee))
		}
	}
}

// leakedCallResultBase reports the allocation a call's result may name, when
// the only arguments the callee lets reach that result are derived from one
// tracked allocation. It is the "leaks only to result" summary made usable: the
// caller keeps asking about the result instead of giving up at the call.
//
// Two tracked allocations reaching one result is a conflict, and both escape --
// the same rule the phi case uses, for the same reason.
func leakedCallResultBase(
	function *ir.Func,
	byName map[string]*ir.Func,
	facts *EscapeFacts,
	instruction ir.Instr,
	bases map[uint32]uint32,
	escaped map[uint32]bool,
) (uint32, bool) {
	if instruction.Op != ir.OCall || instruction.To.Kind != ir.RefTemp {
		return 0, false
	}
	if isAtomicPointerStore(function, instruction) || benignMemoryCall(function, instruction) {
		return 0, false
	}
	callee := constSymbolName(function, instruction.Arg(0))
	target, arguments, usable := summarisedCallee(byName, callee, instruction)
	if !usable {
		return 0, false
	}
	var base uint32
	found := false
	for index, argument := range arguments {
		fact := facts.Param(callee, index)
		if !callLeaksToTrackedResult(instruction, target, fact) {
			continue
		}
		argumentBase, tracked := heapBase(argument, bases)
		if !tracked {
			continue
		}
		if found && argumentBase != base {
			escaped[base] = true
			escaped[argumentBase] = true
			return 0, false
		}
		base = argumentBase
		found = true
	}
	return base, found
}

// calleeRetainsNothing reports //go:noescape on the called symbol: the callee
// keeps no pointer it was handed past the call.
//
// It is checked before the module lookup because the functions that carry the
// directive are exactly the ones with no Go body -- assembly, or a runtime
// intrinsic -- which need not appear as an ir.Func at all.
//
// This is read as the strong claim -- nothing reachable through an argument is
// retained -- where a computed ParamNoEscape is read as the weak one (see
// escapeGraph.call). The difference is deliberate: a directive is a promise
// about the whole argument written by whoever wrote the assembly, which is how
// gc reads it too, whereas a computed summary is a dereference depth this
// analysis derived and must not be rounded up into a stronger promise.
func calleeRetainsNothing(function *ir.Func, callee string) bool {
	if callee == "" {
		return false
	}
	module := function.Module()
	if module == nil {
		return false
	}
	return module.SymAttrOf(callee).Has(ir.SymNoEscape)
}

// summarisedCallee resolves a call to the module function it names together
// with its value arguments, and reports whether the argument list lines up with
// the callee's parameter list one for one.
//
// It has to line up exactly. An aggregate result passed as a result0 parameter
// breaks the identity between argument position and parameter index, and a
// summary read at the wrong index is a wrong answer in the permissive
// direction. The inliner guards its own parameter substitution with the same
// test.
//
// A scalarised aggregate argument does not have to break it. The fact table is
// indexed by ir.Func.Params position, which is the *flattened* list -- a slice
// parameter is three entries there, an interface two -- so when the call
// scalarises its arguments the same way the callee scalarised its parameters,
// position still names the same value on both sides. scalarisedArgumentsAlign
// is that check; without it every call taking a slice or an interface was
// answered conservatively, which is why a variadic call's backing array could
// never be promoted however plainly the callee dropped it.
func summarisedCallee(byName map[string]*ir.Func, callee string, instruction ir.Instr) (*ir.Func, []ir.Ref, bool) {
	arguments := instruction.Args
	if len(arguments) > 0 {
		arguments = arguments[1:]
	}
	if callee == "" || byName == nil {
		return nil, arguments, false
	}
	target := byName[callee]
	if target == nil {
		return nil, arguments, false
	}
	if len(target.Params) != len(arguments) {
		return target, arguments, false
	}
	if !scalarisedArgumentsAlign(target, instruction) {
		return target, arguments, false
	}
	return target, arguments, true
}

// scalarisedArgumentsAlign reports that a call's scalarised aggregate arguments
// occupy exactly the argument-list positions the callee's parameter list gives
// the same values, so argument index and parameter index name the same thing.
//
// Both ir.ValueGroup lists are indexed relative to their own list -- the
// callee's to ir.Func.Params, the call's to Args[1:] -- and the caller has
// already checked those two lists are the same length. Equal (Index, Count)
// runs in the same order therefore means the two flattenings coincide
// everywhere: same starts, same widths, and so the same scalars in between.
//
// A call with no groups at all is left alone rather than compared against the
// callee's, so this can only widen what the summaries answer, never narrow it.
func scalarisedArgumentsAlign(target *ir.Func, instruction ir.Instr) bool {
	if len(instruction.ArgGroups) == 0 {
		return true
	}
	if len(target.ParamGroups) != len(instruction.ArgGroups) {
		return false
	}
	for index, argument := range instruction.ArgGroups {
		parameter := target.ParamGroups[index]
		if parameter.Index != argument.Index || parameter.Count != argument.Count {
			return false
		}
	}
	return true
}

// summaryUnavailableReason says why a call had to be answered conservatively,
// for the shadow-mode report. It is only built when reasons are being
// collected, because mark discards the string otherwise.
func summaryUnavailableReason(callee string, target *ir.Func, instruction ir.Instr) string {
	switch {
	case callee == "":
		return "indirect call"
	case target == nil:
		return "call to $" + callee + ", which is not a module function"
	case len(target.Params) != len(instruction.Args)-1:
		return "call to $" + callee + ", whose argument list does not match its parameters"
	default:
		return "call to $" + callee + " with misaligned scalarised aggregate arguments"
	}
}

// callLeaksToTrackedResult reports that a leaks-to-result summary is one the
// caller can act on: the callee returns one scalar value into one temporary, so
// "the argument escapes exactly when the result does" names something the
// caller can keep tracking.
//
// An aggregate return, a multi-register return, or a result-area parameter all
// mean the call expression does not stand for one value, and the summary is
// unusable rather than wrong -- the caller falls back to escaping the argument.
func callLeaksToTrackedResult(instruction ir.Instr, target *ir.Func, fact ParamFact) bool {
	if fact.Escape != ParamLeaksToResult || fact.Result != 0 {
		return false
	}
	if target.RetAgg != nil || !target.HasRet || len(instruction.Defs) > 0 {
		return false
	}
	if instruction.To.Kind != ir.RefTemp {
		return false
	}
	for _, parameter := range target.Params {
		if parameter != nil && strings.HasPrefix(parameter.Name, "result") {
			return false
		}
	}
	return true
}

// instructionReason names the use that escaped an allocation, for the
// shadow-mode report.
func instructionReason(function *ir.Func, instruction ir.Instr) string {
	if instruction.Op == ir.OCall {
		if callee := constSymbolName(function, instruction.Arg(0)); callee != "" {
			return "call to $" + callee
		}
		return "indirect call"
	}
	return "used by " + instruction.Op.String()
}
