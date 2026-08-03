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
	bases map[uint32]uint32,
	selfReferential map[uint32]bool,
	mark func(ir.Ref, string),
	markContents func(ir.Ref, string),
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
		if !fact.Deep {
			// The depth-1 half of the summary, and the one a variadic parameter
			// needs. ParamNoEscape is a claim about the pointer handed over;
			// what a `...any` callee retains is an *element*, which is the boxed
			// payload the element's data word points at and not the backing
			// array at all. Everything the argument holds therefore escapes,
			// and the argument itself is left to the switch below.
			//
			// markContents walks the containment edges the mark loop recorded,
			// so it names the payloads goc allocated for this call and nothing
			// else. For an argument holding nothing it does nothing, which is
			// why this can run on every call rather than only on the shapes it
			// was written for.
			markContents(argument, fmt.Sprintf("argument %d of $%s may retain something it points at", index, callee))
		}
		if needsDeepSummary(argument, bases, selfReferential) && !fact.Deep {
			// The allocation holds a pointer into itself, so "the callee does
			// not retain the pointer it was handed" is not enough: retaining
			// anything reachable through it retains the object. Only the deep
			// claim answers the question that is actually being asked.
			//
			// This is the case markContents cannot express, because there is no
			// separate object to escape: the payload is a field of the argument.
			// Every payload goc can build with a conversion helper is emitted as
			// an allocation of its own for exactly this reason -- see
			// goc.gen.variadicPayloadStorage.
			mark(argument, fmt.Sprintf("argument %d of $%s may retain something inside a self-referential object", index, callee))
			continue
		}
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

// needsDeepSummary reports that an argument names a tracked allocation that
// contains a pointer into itself, so a depth-0 summary about it is not the
// question the caller needs answered.
//
// storesIntoItself is right that such a store publishes nothing on its own. The
// consequence it carries, though, is that the object's own contents are no
// longer a separate allocation with an escape decision of their own: goc packs a
// variadic `...any` call's backing array and the boxed payloads its elements
// point at into one object, so a callee that retains an *element* -- `sink =
// args[0]` -- retains the whole object. ParamNoEscape says only that the
// pointer handed over is not kept; ParamFact.Deep is the claim that covers this.
func needsDeepSummary(argument ir.Ref, bases map[uint32]uint32, selfReferential map[uint32]bool) bool {
	base, tracked := heapBase(argument, bases)
	return tracked && selfReferential[base]
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
	if len(target.Params) == len(arguments) {
		if !scalarisedArgumentsAlign(target, instruction) {
			return target, arguments, false
		}
		return target, arguments, true
	}
	widened, aligned := argumentsWidenedToParameters(target, instruction, arguments)
	if !aligned {
		return target, arguments, false
	}
	return target, widened, true
}

// argumentsWidenedToParameters lines a call up with its callee when the two
// disagree about which aggregates to scalarise, by repeating an aggregate
// argument once for each parameter it carries.
//
// The disagreement is real and ordinary. goc's parameter builder flattens an
// interface parameter into two words and a slice into three, while its call
// emitter scalarises the slice and hands the interface over as one descriptor
// value -- so `Logger.Info` calling `Logger.log(ctx, level, msg, args...)`
// passes seven values to eight parameters, and every summary about that call
// was thrown away over the one argument that did not line up. The variadic
// backing array is one of the seven.
//
// Repeating the reference is the whole trick. The consumers all walk the
// returned list against parameter index, so an aggregate that appears twice is
// asked about twice and gets whatever the worse of its two parameters says --
// which is exactly the right answer for a value that carries both words. No
// consumer needs to learn that spans exist.
//
// It only runs when the plain counts already fail to match, so a call that was
// answered before is answered identically now.
func argumentsWidenedToParameters(target *ir.Func, instruction ir.Instr, arguments []ir.Ref) ([]ir.Ref, bool) {
	parameterGroups := make(map[int]ir.ValueGroup, len(target.ParamGroups))
	for _, group := range target.ParamGroups {
		parameterGroups[group.Index] = group
	}
	argumentGroups := make(map[int]ir.ValueGroup, len(instruction.ArgGroups))
	for _, group := range instruction.ArgGroups {
		argumentGroups[group.Index] = group
	}

	widened := make([]ir.Ref, 0, len(target.Params))
	parameter := 0
	for index := 0; index < len(arguments); {
		if group, scalarised := argumentGroups[index]; scalarised {
			// The call already broke this aggregate into scalars. The callee has
			// to have broken the same one, at the same place, the same way.
			callee, flattened := parameterGroups[parameter]
			if !flattened || callee.Count != group.Count || callee.Type != group.Type {
				return nil, false
			}
			if group.Count <= 0 || index+group.Count > len(arguments) {
				return nil, false
			}
			widened = append(widened, arguments[index:index+group.Count]...)
			index += group.Count
			parameter += group.Count
			continue
		}
		width := 1
		if aggregate := aggregateArgumentType(instruction, index); aggregate != nil {
			if callee, flattened := parameterGroups[parameter]; flattened {
				if callee.Type != aggregate || callee.Count <= 0 {
					return nil, false
				}
				width = callee.Count
			}
		} else if _, flattened := parameterGroups[parameter]; flattened {
			// The callee scalarised something the call is passing as a plain
			// value, and nothing here says how many parameters it covers.
			return nil, false
		}
		for repeat := 0; repeat < width; repeat++ {
			widened = append(widened, arguments[index])
		}
		index++
		parameter += width
	}
	if parameter != len(target.Params) || len(widened) != len(target.Params) {
		return nil, false
	}
	return widened, true
}

// aggregateArgumentType names the aggregate one value argument carries, or nil
// when the argument is an ordinary scalar. ir.Instr.AggArgs is indexed over the
// value arguments, which is the same indexing the callers use.
func aggregateArgumentType(instruction ir.Instr, index int) *ir.AggType {
	if index < 0 || index >= len(instruction.AggArgs) {
		return nil
	}
	return instruction.AggArgs[index]
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
