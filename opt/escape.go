package opt

import (
	"os"

	"github.com/evanphx/cg12/ir"
)

type localSlot struct {
	base   locKey
	offset int64
}

// EscapeSummaries turns on the cross-function fact table inside
// LowerHeapAllocations. It is on, and GOC_ESCAPE_SUMMARIES=0 turns it off.
//
// The knob is kept for bisection, which is the only thing it is for. With it off
// LowerHeapAllocations is handed a nil table and every call falls into the same
// assume-the-worst arm it fell into before summaries existed: the pass cannot
// tell it is running under a build that has them. So a placement that looks
// wrong can be attributed to the table or cleared of it in one run, without
// rebuilding the compiler.
//
// It does not turn off the loop rule, which is a safety property of promotion
// rather than part of the table -- see promotionsBlockedByALoop.
var EscapeSummaries = os.Getenv("GOC_ESCAPE_SUMMARIES") != "0"

// HeapAllocLowering counts what LowerHeapAllocations did with the candidates it
// saw, which is the number that says whether the pass got smarter or just got
// more work.
type HeapAllocLowering struct {
	Promoted int // candidates that became frame slots
	Lowered  int // candidates that became allocator calls
	// LoopBlocked counts the candidates inside Lowered that the escape analysis
	// was willing to promote and the loop rule sent to the allocator anyway. It
	// is the price of promotionsBlockedByALoop, and the only number that says
	// whether the rule is doing anything.
	LoopBlocked int
}

// Rate is the share of candidates promoted to a frame slot.
func (stats HeapAllocLowering) Rate() float64 {
	total := stats.Promoted + stats.Lowered
	if total == 0 {
		return 0
	}
	return float64(stats.Promoted) / float64(total)
}

// HeapAllocLoweringStats reads the promotion counts back out of a compiled
// module. LowerHeapAllocations records one AllocDecision per candidate, so this
// is a count of that record rather than a second instrument.
func HeapAllocLoweringStats(module *ir.Module) HeapAllocLowering {
	var stats HeapAllocLowering
	for _, decision := range module.AllocDecisions {
		if decision.Placement == ir.AllocInFrame {
			stats.Promoted++
			continue
		}
		stats.Lowered++
		if decision.BlockedByLoop {
			stats.LoopBlocked++
		}
	}
	return stats
}

// LowerHeapAllocations promotes typed heap-allocation candidates whose
// pointers provably remain local to stack slots. Candidates that may escape
// are lowered to ordinary allocator calls.
//
// Each decision is appended to module.AllocDecisions as it is made. That record
// is diagnostic only -- nothing in the compiler reads it -- but it is the only
// place the frame half of the decision survives: a promoted candidate becomes a
// bare OAlloc carrying a byte size and nothing else, so after this pass runs
// there is no way to ask the IR which frame slots used to be allocations, or of
// what type. See AllocationCensus.
func LowerHeapAllocations(module *ir.Module) bool {
	var facts *EscapeFacts
	if EscapeSummaries {
		facts = ComputeEscapeFacts(module)
	}
	return lowerHeapAllocations(module, facts)
}

// LowerHeapAllocationsWithFacts is LowerHeapAllocations handed a summary table
// computed elsewhere, so a caller that already has one -- or that wants to
// measure with and without -- does not pay for it twice. A nil table is the
// no-summaries configuration and behaves exactly as the pass always has.
func LowerHeapAllocationsWithFacts(module *ir.Module, facts *EscapeFacts) bool {
	return lowerHeapAllocations(module, facts)
}

func lowerHeapAllocations(module *ir.Module, facts *EscapeFacts) bool {
	byName := moduleFuncsByName(module, facts)
	changed := false
	for _, function := range module.Funcs {
		if function.Start == nil {
			continue
		}
		if lowerFunctionHeapAllocations(function, byName, facts, &module.AllocDecisions) {
			changed = true
		}
	}
	return changed
}

// moduleFuncsByName indexes a module's functions for summary lookups. Without a
// table there is nothing to look up, so it is not built.
func moduleFuncsByName(module *ir.Module, facts *EscapeFacts) map[string]*ir.Func {
	if facts == nil {
		return nil
	}
	byName := make(map[string]*ir.Func, len(module.Funcs))
	for _, function := range module.Funcs {
		byName[function.Name] = function
	}
	return byName
}

// candidateEscapes is the per-function may-analysis behind the pass: which of a
// given set of allocations may have their address outlive the frame.
//
// It is separated from the rewrite so the same analysis can be asked about
// allocations the front end already placed, which is what ShadowPlacement does.
// Seeding it with the front-end's own allocations rather than with this
// function's OHeapAlloc candidates is the only difference between the two uses.
type candidateEscapes struct {
	function *ir.Func
	bases    map[uint32]uint32
	escaped  map[uint32]bool
	// contains records, per tracked allocation, the other tracked allocations
	// whose address was written into it. See containedAllocationsEscape.
	contains map[uint32]map[uint32]bool
	// reasons names, per escaped allocation, the first use that escaped it, and
	// reasonPositions is where that use is written. Both are only filled when
	// asked for: the pass itself does not need them and the strings would be
	// built on every compile.
	reasons         map[uint32]string
	reasonPositions map[uint32]ir.SrcPos
}

func (analysis *candidateEscapes) escapes(id uint32) bool { return analysis.escaped[id] }

// containedAllocationsEscape propagates escape through containment: an object
// reachable from one that escapes has escaped too.
//
// Writing one tracked allocation's address into another is not a publication on
// its own -- nothing outside can reach the pointee any more easily than it can
// reach the container -- so the mark loop records the edge instead of escaping
// the pointee. That is what lets a variadic call's `[N]any` backing array stay
// in the frame while the boxed payload one of its elements points at goes to the
// heap, which is the placement gc makes and the one goc could not express while
// the array and its payloads were a single object.
//
// The debt that defers is settled here, and it has to be settled every time
// anything is added to escaped: promotionsBlockedByALoop escapes candidates
// after the mark loop has finished, and a container sent to the heap by the loop
// rule with a promoted pointee inside it is a frame address published into a
// heap object -- exactly what TestFrameEscapeAudit exists to catch.
func (analysis *candidateEscapes) containedAllocationsEscape() {
	if len(analysis.contains) == 0 {
		return
	}
	for updated := true; updated; {
		updated = false
		for container, contained := range analysis.contains {
			if !analysis.escaped[container] {
				continue
			}
			for base := range contained {
				if analysis.escaped[base] {
					continue
				}
				if analysis.reasons != nil {
					analysis.reasons[base] = "stored into an object that escapes"
					analysis.reasonPositions[base] = analysis.reasonPositions[container]
				}
				analysis.escaped[base] = true
				updated = true
			}
		}
	}
}

func (analysis *candidateEscapes) reason(id uint32) string {
	if analysis.reasons == nil {
		return ""
	}
	return analysis.reasons[id]
}

// reasonPosition is where the use named by reason is written, when the marking
// instruction carried a position.
func (analysis *candidateEscapes) reasonPosition(id uint32) ir.SrcPos {
	if analysis.reasonPositions == nil {
		return ir.SrcPos{}
	}
	return analysis.reasonPositions[id]
}

func lowerFunctionHeapAllocations(function *ir.Func, byName map[string]*ir.Func, facts *EscapeFacts, decisions *[]ir.AllocDecision) bool {
	var seeds []uint32
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.Op == ir.OHeapAlloc && instruction.To.Kind == ir.RefTemp {
				seeds = append(seeds, instruction.To.ID)
			}
		}
	}
	if len(seeds) == 0 {
		return false
	}
	// Reasons are asked for exactly when the diagnostic is on. They are written
	// as the mark loop marks and read back by the diagnostic; nothing in the
	// pass consults them, and with the diagnostic off the map is never
	// allocated. See EscapeDiagLevel.
	escapes := analyzeCandidateEscapes(function, byName, facts, seeds, EscapeDiagLevel() > 0)
	blocked := promotionsBlockedByALoop(function, seeds, escapes)
	// After the loop rule, because the loop rule is one of the ways a payload
	// ends up escaping out of a framed array, and the count it is comparing has
	// to be the final one.
	foldSplitPayloadsBackIn(function, escapes)
	rewriteHeapAllocations(function, escapes, blocked, decisions)
	return true
}

// promotionsBlockedByALoop escapes every candidate the analysis was willing to
// promote that sits inside a natural loop, and reports which ones it blocked.
//
// Neither this analysis nor goc's AST walk has a notion of iteration. Both
// answer "does a pointer outlive the frame", and an allocation in a loop body
// can be entirely frame-local by that test and still need one object per
// iteration. Promoting it puts every iteration on one slot, so two objects the
// source says are distinct become the same object: goc prints `2 2` where Go
// prints `1 2`. RUNTIME_PLAN.md section 5.9 is what freshVariableStorage exists
// for on the front end's side of the same problem.
//
// opt.FrameEscapes is structurally blind to this. It asks whether a frame
// address was published past its frame, and in the aliasing case none is -- both
// pointers stay inside the frame and are simply the same pointer. A clean audit
// says nothing about it, which is why the rule is here rather than left to be
// caught downstream.
//
// It applies whether or not the summaries are on. The rule is about the loop,
// not about how the analysis came to be willing to promote; every promotion is a
// placement change and every placement change in a loop body is this hazard.
func promotionsBlockedByALoop(function *ir.Func, seeds []uint32, escapes *candidateEscapes) map[uint32]bool {
	promotable := make(map[uint32]bool, len(seeds))
	for _, seed := range seeds {
		if !escapes.escapes(seed) {
			promotable[seed] = true
		}
	}
	if len(promotable) == 0 {
		// Nothing was going to be promoted, so there is no CFG to build. This is
		// the common case: most candidates reach something that escapes them.
		return nil
	}
	blocked, loopHeaders := allocationsInLoops(function, promotable)
	for id := range blocked {
		if escapes.reasons != nil {
			escapes.reasons[id] = "allocated in a loop; one frame slot cannot hold one object per iteration"
			escapes.reasonPositions[id] = loopHeaders[id]
		}
		escapes.escaped[id] = true
	}
	// The loop rule adds to escaped after the mark loop has finished, and a
	// container it sends to the heap takes everything inside it along. Leaving
	// that unsettled would promote a payload into a frame slot and leave its
	// address inside a heap object -- a frame-address publication, and the one
	// direction this analysis must never get wrong.
	escapes.containedAllocationsEscape()
	return blocked
}

// analyzeCandidateEscapes runs the may-analysis over one function for the
// allocations named by seeds.
func analyzeCandidateEscapes(function *ir.Func, byName map[string]*ir.Func, facts *EscapeFacts, seeds []uint32, wantReasons bool) *candidateEscapes {
	aliases := newAliasInfo(function)
	// goc gives an aggregate local a slot holding the address of its backing
	// storage, and every use of the variable reads that slot back. aliasInfo
	// answers cUnknown for the pointer that comes out, so without this a store
	// into a local array or struct reads as a store into storage the analysis
	// does not own. See opt/escapeindirect.go.
	indirect := resolveIndirectStorage(function, aliases)
	locOf := func(reference ir.Ref, width int) locInfo {
		return escapeLocOf(aliases, indirect, reference, width)
	}
	definitions := make(map[uint32]ir.Instr)
	bases := make(map[uint32]uint32)
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			if instruction.To.Kind == ir.RefTemp {
				definitions[instruction.To.ID] = instruction
			}
		}
	}
	for _, seed := range seeds {
		bases[seed] = seed
	}

	escaped := make(map[uint32]bool)
	contains := declaredContainment(function, bases)
	var reasons map[uint32]string
	var reasonPositions map[uint32]ir.SrcPos
	if wantReasons {
		reasons = make(map[uint32]string)
		reasonPositions = make(map[uint32]ir.SrcPos)
	}
	slotBases := make(map[localSlot]uint32)
	conflictedSlots := make(map[localSlot]bool)
	for updated := true; updated; {
		updated = false
		for id, instruction := range definitions {
			if known, tracked := bases[id]; tracked {
				// A call result can gain a second tracked argument after the
				// round that first named it -- an argument whose own base
				// arrives through a slot load, say, which the loop below
				// resolves. Two allocations reaching one result is a conflict
				// whenever it becomes visible, not only when it is visible
				// first: leaving the result bound to whichever one was resolved
				// earliest escapes that one and silently promotes the other,
				// which the call may equally well have returned.
				if facts != nil {
					if base, ok := leakedCallResultBase(function, byName, facts, instruction, bases, escaped); ok && base != known {
						escaped[base] = true
						escaped[known] = true
					}
				}
				continue
			}
			base, ok := derivedHeapBase(instruction, bases)
			if !ok && facts != nil {
				base, ok = leakedCallResultBase(function, byName, facts, instruction, bases, escaped)
			}
			if ok {
				bases[id] = base
				updated = true
			}
		}
		for _, block := range function.Blocks {
			for _, phi := range block.Phis {
				base, found, conflict := phiHeapBase(phi, bases)
				if conflict {
					for _, argument := range phi.Args {
						if argumentBase, tracked := heapBase(argument, bases); tracked {
							escaped[argumentBase] = true
						}
					}
					continue
				}
				if !found || phi.To.Kind != ir.RefTemp {
					continue
				}
				if previous, exists := bases[phi.To.ID]; exists {
					if previous != base {
						escaped[previous] = true
						escaped[base] = true
					}
					continue
				}
				bases[phi.To.ID] = base
				updated = true
			}
		}
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				destination, source, size, memoryCopy := memoryCopyOperands(function, instruction)
				if memoryCopy {
					sourceLocation := locOf(source, int(size))
					destinationLocation := locOf(destination, int(size))
					if sourceLocation.class == cLocal {
						for sourceSlot, base := range slotBases {
							if sourceSlot.base != sourceLocation.key() {
								continue
							}
							if sourceSlot.offset < sourceLocation.offset || sourceSlot.offset+8 > sourceLocation.offset+size {
								continue
							}
							if destinationLocation.class != cLocal {
								// The copy that puts a boxed payload into its
								// container: goc builds a variadic `...any`
								// element by copying a two-word interface
								// descriptor out of a frame slot into the
								// backing array, and the payload pointer rides
								// along in it. That is containment, declared by
								// the candidate itself, not a publication.
								if !declaredContainedBase(contains, base, destination, bases) {
									escaped[base] = true
								}
								continue
							}
							destinationSlot := localSlot{
								base:   destinationLocation.key(),
								offset: destinationLocation.offset + sourceSlot.offset - sourceLocation.offset,
							}
							if conflictedSlots[destinationSlot] {
								escaped[base] = true
								continue
							}
							if previous, exists := slotBases[destinationSlot]; exists && previous != base {
								escaped[previous] = true
								escaped[base] = true
								delete(slotBases, destinationSlot)
								conflictedSlots[destinationSlot] = true
								continue
							}
							if _, exists := slotBases[destinationSlot]; !exists {
								slotBases[destinationSlot] = base
								updated = true
							}
						}
					}
				}
				if value, address, isPointerStore := trackedPointerStore(function, instruction); isPointerStore {
					base, tracked := heapBase(value, bases)
					location := locOf(address, 1)
					if !tracked || location.class != cLocal {
						continue
					}
					slot := localSlot{base: location.key(), offset: location.offset}
					if conflictedSlots[slot] {
						escaped[base] = true
						continue
					}
					if previous, exists := slotBases[slot]; exists && previous != base {
						escaped[previous] = true
						escaped[base] = true
						delete(slotBases, slot)
						conflictedSlots[slot] = true
						continue
					}
					if _, exists := slotBases[slot]; !exists {
						slotBases[slot] = base
						updated = true
					}
					continue
				}
				if !instruction.Op.IsLoad() || instruction.To.Kind != ir.RefTemp {
					continue
				}
				location := locOf(instruction.Arg(0), 1)
				if location.class != cLocal {
					continue
				}
				slot := localSlot{base: location.key(), offset: location.offset}
				base, tracked := slotBases[slot]
				if !tracked {
					continue
				}
				if previous, exists := bases[instruction.To.ID]; exists && previous != base {
					escaped[previous] = true
					escaped[base] = true
					continue
				}
				if _, exists := bases[instruction.To.ID]; !exists {
					bases[instruction.To.ID] = base
					updated = true
				}
			}
		}
	}

	// Read again now that the fixed point has resolved every base a container
	// could be named through. The first reading, before the loop, is what the
	// block-copy case above needed; this one is what the mark loop gets.
	contains = declaredContainment(function, bases)

	// Which tracked allocations hold a pointer into themselves. Collected in its
	// own pass rather than as the mark loop meets them, because a call can
	// precede in program order the store that makes its argument
	// self-referential, and the answer has to be the same either way.
	selfReferential := make(map[uint32]bool)
	noteSelfReference := func(value, destination ir.Ref) {
		if base, tracked := heapBase(value, bases); tracked && storesIntoItself(value, destination, bases) {
			selfReferential[base] = true
		}
	}
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			switch {
			case isAtomicPointerStore(function, instruction):
				noteSelfReference(instruction.Arg(2), instruction.Arg(1))
			case instruction.Op.IsStore():
				noteSelfReference(instruction.Arg(0), instruction.Arg(1))
			}
		}
	}

	// at is the position of the instruction being marked from, so that the
	// diagnostic can point at the use rather than only name it. It is the
	// marking instruction's own position, taken here rather than looked up
	// afterwards -- the rewrite below replaces these instructions.
	mark := func(reference ir.Ref, why string, at ir.SrcPos) {
		if reference.Kind == ir.RefTemp {
			if base, ok := bases[reference.ID]; ok {
				if reasons != nil && !escaped[base] {
					reasons[base] = why
					reasonPositions[base] = at
				}
				escaped[base] = true
			}
		}
	}
	// markContents escapes what a tracked allocation holds without escaping the
	// allocation. It is the depth-1 half of a summary: a callee that may retain
	// an *element* of a `...any` slice retains the boxed payload the element
	// points at, and nothing else. Transitive, because containment is.
	markContents := func(reference ir.Ref, why string, at ir.SrcPos) {
		base, tracked := heapBase(reference, bases)
		if !tracked {
			return
		}
		frontier := []uint32{base}
		for len(frontier) > 0 {
			container := frontier[len(frontier)-1]
			frontier = frontier[:len(frontier)-1]
			for held := range contains[container] {
				if escaped[held] {
					continue
				}
				if reasons != nil {
					reasons[held] = why
					reasonPositions[held] = at
				}
				escaped[held] = true
				frontier = append(frontier, held)
			}
		}
	}
	for _, block := range function.Blocks {
		for _, phi := range block.Phis {
			if _, tracked := heapBase(phi.To, bases); tracked {
				continue
			}
			for _, argument := range phi.Args {
				// A phi carries no position of its own; the reason names the
				// construct and there is no instruction to point at.
				mark(argument, "phi", ir.SrcPos{})
			}
		}
		for _, instruction := range block.Instrs {
			switch {
			case instruction.Op == ir.OHeapAlloc:
				// The allocator, type descriptor, size, and -- on a converted
				// candidate -- the helper and the value it is handed are not
				// uses of the result. The value is one the helper copies, never
				// a pointer the object could be reached through: only a
				// register-shaped, pointer-free type has a helper at all.
			case instruction.Op.IsLoad():
				// Reading through the candidate keeps it local -- but reading a
				// declared payload's pointer back *out* of its container does
				// not, because the loaded value is not tracked and nothing marks
				// what is done with it afterwards. goc never emits that for the
				// shape this exists for: it fills the backing array and hands it
				// straight to the callee. If some later pass ever does, the
				// contents escape here rather than silently outliving the frame.
				markContents(instruction.Arg(0), "read back out of the object holding it", instruction.Pos)
			case instruction.Op == ir.OCmp:
				// Comparing a pointer observes its value but cannot retain it.
			case instruction.Op.IsStore():
				// Frontend variables live in ordinary stack allocations. Saving a
				// candidate in a non-escaping local slot does not make the pointed-to
				// object escape; storing it anywhere externally reachable does.
				//
				// Storing it into another tracked allocation is neither: it is
				// containment, recorded before this loop ran, and settled by
				// containedAllocationsEscape once the container's own answer is
				// known.
				if locOf(instruction.Arg(1), 1).class != cLocal &&
					!storesIntoItself(instruction.Arg(0), instruction.Arg(1), bases) &&
					!declaredContained(contains, instruction.Arg(0), instruction.Arg(1), bases) {
					mark(instruction.Arg(0), "store into non-local storage", instruction.Pos)
				}
			case facts != nil && instruction.Op == ir.OCall &&
				!isAtomicPointerStore(function, instruction) && !benignMemoryCall(function, instruction):
				// The summarised form of the default arm below: an argument is only
				// a publication when the callee's summary says the callee can retain
				// it. This is the entire behavioural difference the fact table makes,
				// and with facts nil the case cannot be reached.
				at := instruction.Pos
				markSummarisedCall(function, byName, facts, instruction, bases, selfReferential,
					func(reference ir.Ref, why string) { mark(reference, why, at) },
					func(reference ir.Ref, why string) { markContents(reference, why, at) })
			case isTrackedHeapDerivation(instruction, bases):
				// Copies, casts, and constant pointer offsets preserve locality.
			case isAtomicPointerStore(function, instruction):
				// The destination is observed only for the duration of the store.
				// This is the write-barrier form emitted for pointer fields before
				// escape lowering knows whether the enclosing object is stack-local.
				destination := instruction.Arg(1)
				if _, localCandidate := heapBase(destination, bases); localCandidate {
					if !storesIntoItself(instruction.Arg(2), destination, bases) &&
						!declaredContained(contains, instruction.Arg(2), destination, bases) {
						mark(instruction.Arg(2), "write barrier into a candidate", instruction.Pos)
					}
				} else if locOf(destination, 1).class != cLocal {
					mark(instruction.Arg(2), "write barrier into non-local storage", instruction.Pos)
				}
			case benignMemoryCall(function, instruction):
				// memcpy/memset observe the storage but do not retain its
				// address. A block copy *out* of a container is a read-out all
				// the same, and is answered the same way the load case is.
				for _, argument := range memoryCallReadsOut(function, instruction) {
					markContents(argument, "block-copied out of the object holding it", instruction.Pos)
				}
			default:
				for _, argument := range instruction.Args {
					mark(argument, instructionReason(function, instruction), instruction.Pos)
				}
			}
		}
		// A block terminator carries no position either.
		mark(block.Jmp.Arg, "returned", ir.SrcPos{})
		for _, argument := range block.Jmp.Args {
			mark(argument, "returned", ir.SrcPos{})
		}
	}
	analysis := &candidateEscapes{
		function:        function,
		bases:           bases,
		escaped:         escaped,
		contains:        contains,
		reasons:         reasons,
		reasonPositions: reasonPositions,
	}
	analysis.containedAllocationsEscape()
	return analysis
}

// declaredContainment reads the containment edges out of the candidates that
// declare them: a candidate emitted by ir.Block.HeapAllocConvertedField says
// which allocation reserved storage for it, and therefore which allocation it is
// logically a field of.
//
// Containment is taken from the declaration rather than inferred from stores on
// purpose. Inferring it -- "a tracked pointer written into another tracked
// allocation is contained, not published" -- is true, and it is *not* enough on
// its own: a pointer loaded back out of the container and published from there
// would then escape neither. Every place a container's contents can be read out
// would have to be found and answered, and getting that list wrong is a dangling
// pointer rather than a lost optimisation. The declared form is a closed set --
// goc emits these candidates at one site, for one shape, and passes the
// container straight to the callee without ever reading it back -- and
// containerContentsAreReadOut is the check that the shape really is that closed.
func declaredContainment(function *ir.Func, bases map[uint32]uint32) map[uint32]map[uint32]bool {
	var contains map[uint32]map[uint32]bool
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			container, _, folds := foldsIntoContainer(function, instruction)
			if !folds || instruction.To.Kind != ir.RefTemp {
				continue
			}
			containerBase, tracked := heapBase(container, bases)
			if !tracked || containerBase == instruction.To.ID {
				continue
			}
			if contains == nil {
				contains = make(map[uint32]map[uint32]bool, 1)
			}
			held := contains[containerBase]
			if held == nil {
				held = make(map[uint32]bool, 1)
				contains[containerBase] = held
			}
			held[instruction.To.ID] = true
		}
	}
	return contains
}

// declaredContained reports that a store writes a declared payload's address
// into the container that declared it. The mark loop asks so it can leave the
// value alone: containedAllocationsEscape settles it later, once the container's
// own answer is known.
func declaredContained(contains map[uint32]map[uint32]bool, value, destination ir.Ref, bases map[uint32]uint32) bool {
	if len(contains) == 0 {
		return false
	}
	valueBase, valueTracked := heapBase(value, bases)
	if !valueTracked {
		return false
	}
	return declaredContainedBase(contains, valueBase, destination, bases)
}

func declaredContainedBase(contains map[uint32]map[uint32]bool, valueBase uint32, destination ir.Ref, bases map[uint32]uint32) bool {
	destinationBase, destinationTracked := heapBase(destination, bases)
	if !destinationTracked || destinationBase == valueBase {
		return false
	}
	return contains[destinationBase][valueBase]
}

// rewriteHeapAllocations legalises every candidate in function according to the
// analysis: promoted candidates become frame allocations with the zeroing the
// allocator used to do, and the rest become the allocator call the instruction
// was already shaped like -- or, for a candidate that named a conversion
// helper, the call to that helper.
func rewriteHeapAllocations(function *ir.Func, analysis *candidateEscapes, loopBlocked map[uint32]bool, decisions *[]ir.AllocDecision) {
	escaped := analysis.escaped
	for _, block := range function.Blocks {
		lowered := make([]ir.Instr, 0, len(block.Instrs))
		droppedStore := -1
		for index, original := range block.Instrs {
			if index == droppedStore {
				continue
			}
			instruction := original
			if instruction.Op != ir.OHeapAlloc || instruction.To.Kind != ir.RefTemp {
				lowered = append(lowered, instruction)
				continue
			}
			if container, offset, folds := foldsIntoContainer(function, original); folds {
				containerBase, tracked := heapBase(container, analysis.bases)
				if tracked && escaped[containerBase] {
					// The container is on the heap and has storage reserved for
					// this object, so being a separate allocation buys nothing.
					// The initializing store is left where it is: unlike the
					// helper path below there is no call that has already
					// written the value.
					//
					// No decision is recorded: a folded payload is not an
					// allocation site any more, it is a field of one, and the
					// container's own decision already says where it went. That
					// keeps the census for a merged variadic call exactly what it
					// was before payloads could be split out at all.
					instruction.Op = ir.OAdd
					instruction.Cls = ir.ClsP
					instruction.Args = []ir.Ref{container, function.Long(offset)}
					instruction.Aux = 0
					lowered = append(lowered, instruction)
					continue
				}
			}
			if escaped[instruction.To.ID] {
				if convertsItsObject(original) && initializesCandidate(block, index+1, original.To) {
					// The helper returns storage already holding the value, so
					// the initializing store has nothing left to do -- and must
					// not run, because for a small value the helper hands back a
					// pointer into a read-only table.
					//
					// The decision is recorded against the helper rather than
					// against the allocator the candidate also carries, because
					// the allocator is now the road not taken and the census has
					// no other way to know which sites these are.
					recordConvertedAllocDecision(decisions, function, original, loopBlocked[instruction.To.ID], analysis.reason(instruction.To.ID), analysis.reasonPosition(instruction.To.ID))
					droppedStore = index + 1
					instruction.Op = ir.OCall
					instruction.Args = []ir.Ref{original.Args[3], original.Args[4]}
					instruction.Aux = 0
					lowered = append(lowered, instruction)
					continue
				}
				recordAllocDecision(decisions, function, original, ir.AllocOnHeap, loopBlocked[instruction.To.ID], analysis.reason(instruction.To.ID), analysis.reasonPosition(instruction.To.ID))
				instruction.Op = ir.OCall
				instruction.Args = instruction.Args[:2]
				instruction.Aux = 0
				lowered = append(lowered, instruction)
				continue
			}
			recordAllocDecision(decisions, function, original, ir.AllocInFrame, false, "", ir.SrcPos{})

			size := instruction.Args[2]
			switch instruction.Aux {
			case 4:
				instruction.Op = ir.OAlloc4
			case 8:
				instruction.Op = ir.OAlloc8
			case 16:
				instruction.Op = ir.OAlloc16
			default:
				panic("opt: invalid heap allocation alignment")
			}
			instruction.Args = []ir.Ref{size}
			instruction.Aux = 0
			lowered = append(lowered, instruction)
			lowered = append(lowered, ir.Instr{
				Op:   ir.OCall,
				Cls:  ir.ClsW,
				Args: []ir.Ref{function.Sym("goc_memset", 0), instruction.To, function.Word(0), size},
				Pos:  instruction.Pos,
			})
		}
		block.Instrs = lowered
	}
}

// convertsItsObject reports whether a candidate names a runtime conversion
// helper that can build its object without an allocator. ir.Block's
// HeapAllocConverted is what puts one there, and its doc comment carries the
// contract; everything this function needs is the two extra arguments.
func convertsItsObject(candidate ir.Instr) bool {
	return len(candidate.Args) >= 5
}

// foldsIntoContainer reports whether a candidate has storage reserved for it
// inside another allocation, and names that allocation and the byte offset. See
// ir.Block.HeapAllocConvertedField.
func foldsIntoContainer(function *ir.Func, candidate ir.Instr) (ir.Ref, int64, bool) {
	if candidate.Op != ir.OHeapAlloc || len(candidate.Args) != 7 {
		return ir.R, 0, false
	}
	offset, ok := constIntValue(function, candidate.Args[6])
	if !ok {
		return ir.R, 0, false
	}
	return candidate.Args[5], offset, true
}

// constIntValue reads an integer constant out of a reference.
func constIntValue(function *ir.Func, reference ir.Ref) (int64, bool) {
	if reference.Kind != ir.RefConst || int(reference.ID) >= len(function.Consts) {
		return 0, false
	}
	constant := function.Consts[reference.ID]
	if constant.Sym != "" {
		return 0, false
	}
	return constant.Int, true
}

// foldSplitPayloadsBackIn decides, per variadic call, whether separating the
// boxed payloads from the `[N]any` array paid, and gives back the ones that did
// not by escaping the array they came out of.
//
// Splitting is free when the array stays in the frame and the payload stays with
// it, and it is a win when the array stays in the frame and one payload leaves:
// the payload is at most one allocation on its own, against one for the whole
// combined object, and a value inside runtime.staticuint64s makes it none.
//
// It stops being a win at two. K escaping payloads out of a framed array cost K
// allocations where the combined object costs exactly one, so past one the
// arithmetic reverses and the array is sent to the heap to take them back.
// `log/slog`'s three-large-integer call is the case that made this necessary:
// it costs one allocation combined and four split, against gc's three.
//
// The upper bound is what is compared, not the expected cost. A conversion
// helper allocates nothing for a small value, so K split payloads may well cost
// nothing at all -- but which values a call site sees is not something a
// compiler knows, and choosing the representation on a guess about them would
// make the cost of a `%d` depend on how big the number turned out to be.
func foldSplitPayloadsBackIn(function *ir.Func, escapes *candidateEscapes) {
	var containers []uint32
	escapingPayloads := make(map[uint32]int)
	for _, block := range function.Blocks {
		for _, instruction := range block.Instrs {
			container, _, folds := foldsIntoContainer(function, instruction)
			if !folds || instruction.To.Kind != ir.RefTemp {
				continue
			}
			base, tracked := heapBase(container, escapes.bases)
			if !tracked || escapes.escaped[base] || !escapes.escaped[instruction.To.ID] {
				continue
			}
			if _, seen := escapingPayloads[base]; !seen {
				containers = append(containers, base)
			}
			escapingPayloads[base]++
		}
	}
	// Ordered by first appearance rather than by map iteration, so the decisions
	// a module's compile makes do not depend on hash order.
	folded := false
	for _, base := range containers {
		if escapingPayloads[base] < 2 {
			continue
		}
		if escapes.reasons != nil {
			escapes.reasons[base] = "holds more separately-allocated payloads than it costs to hold them"
		}
		escapes.escaped[base] = true
		folded = true
	}
	if folded {
		escapes.containedAllocationsEscape()
	}
}

// initializesCandidate reports whether the instruction at index is the store
// that gives a freshly allocated candidate its first value.
//
// The candidate's result is a temporary the emitter has just made, so a store
// through it is the initialization and nothing else; requiring the store to sit
// immediately after the candidate is what makes that true of the *only* store
// this looks at, rather than of some later write the emitter did not promise
// anything about.
func initializesCandidate(block *ir.Block, index int, candidate ir.Ref) bool {
	if index >= len(block.Instrs) {
		return false
	}
	store := block.Instrs[index]
	return store.Op.IsStore() && len(store.Args) == 2 && store.Args[1] == candidate
}

// storesIntoItself reports a store whose value and whose destination are both
// derived from the same tracked allocation: the object is being made to point
// at part of itself.
//
// Nothing outside can reach the object any more easily for it, so the store
// says nothing about whether the object escapes -- "it escapes if it escapes"
// is all it means. Marking it a publication instead is what kept a variadic
// `...any` call's backing object on the heap: the front end packs the `[]any`
// array and the boxed payloads it points at into one allocation, so writing
// each element's data word writes a pointer into that object, into that same
// object.
func storesIntoItself(value, destination ir.Ref, bases map[uint32]uint32) bool {
	valueBase, valueTracked := heapBase(value, bases)
	destinationBase, destinationTracked := heapBase(destination, bases)
	return valueTracked && destinationTracked && valueBase == destinationBase
}

// recordAllocDecision notes where one heap-allocation candidate landed.
// candidate must still be the unrewritten OHeapAlloc, whose first two arguments
// are the allocator and the type descriptor.
func recordAllocDecision(decisions *[]ir.AllocDecision, function *ir.Func, candidate ir.Instr, placement ir.AllocPlacement, blockedByLoop bool, reason string, use ir.SrcPos) {
	if decisions == nil {
		return
	}
	*decisions = append(*decisions, ir.AllocDecision{
		Func:          function.Name,
		Pos:           candidate.Pos,
		Allocator:     constSymbolName(function, candidate.Args[0]),
		Type:          constSymbolName(function, candidate.Args[1]),
		Placement:     placement,
		BlockedByLoop: blockedByLoop,
		Reason:        reason,
		Use:           use,
	})
}

// recordConvertedAllocDecision notes a candidate that left the frame and was
// built by its conversion helper rather than by an allocator. The placement is
// AllocOnHeap because the object did not stay in the frame, which is the
// decision this record is about; whether the helper allocates for a given value
// is up to the value, and opt.conversionHelpers is where that is written down.
func recordConvertedAllocDecision(decisions *[]ir.AllocDecision, function *ir.Func, candidate ir.Instr, blockedByLoop bool, reason string, use ir.SrcPos) {
	if decisions == nil {
		return
	}
	*decisions = append(*decisions, ir.AllocDecision{
		Func:          function.Name,
		Pos:           candidate.Pos,
		Allocator:     constSymbolName(function, candidate.Args[3]),
		Type:          constSymbolName(function, candidate.Args[1]),
		Placement:     ir.AllocOnHeap,
		BlockedByLoop: blockedByLoop,
		Reason:        reason,
		Use:           use,
	})
}

// constSymbolName returns the symbol name a reference names, or "" if it is not
// a symbol constant.
func constSymbolName(function *ir.Func, reference ir.Ref) string {
	if reference.Kind != ir.RefConst || int(reference.ID) >= len(function.Consts) {
		return ""
	}
	constant := function.Consts[reference.ID]
	if constant.Kind != ir.ConstSym {
		return ""
	}
	return constant.Sym
}

func phiHeapBase(phi *ir.Phi, bases map[uint32]uint32) (uint32, bool, bool) {
	var base uint32
	found := false
	for _, argument := range phi.Args {
		argumentBase, tracked := heapBase(argument, bases)
		if !tracked {
			continue
		}
		if found && argumentBase != base {
			return 0, false, true
		}
		base = argumentBase
		found = true
	}
	return base, found, false
}

// trackedPointerStore names the value and the destination address of a store
// the base propagation has to follow.
//
// goc writes a pointer into a frame slot two ways: a plain store, and a call to
// the write barrier, which is what it emits for every pointer field of every
// pointer-bearing local before escape lowering knows whether the enclosing
// object is stack-local. Both put a candidate in that slot, and a later load
// recovers it.
//
// Modelling only the first is how a candidate used to lose its identity. The
// marking switch in analyzeCandidateEscapes already understands the barrier --
// isAtomicPointerStore is one of its cases -- so the two halves of the same
// analysis disagreed about what a barrier is: a candidate stored through one
// got no slotBases entry, the reload got no base, and every use of the reloaded
// pointer, including a publication into a heap object, was invisible. See
// TestLowerHeapAllocationsTracksEscapeThroughAWriteBarrieredLocalSlot.
func trackedPointerStore(function *ir.Func, instruction ir.Instr) (value, address ir.Ref, ok bool) {
	if instruction.Op.IsStore() {
		return instruction.Arg(0), instruction.Arg(1), true
	}
	if isAtomicPointerStore(function, instruction) {
		return instruction.Arg(2), instruction.Arg(1), true
	}
	return ir.R, ir.R, false
}

func isAtomicPointerStore(function *ir.Func, instruction ir.Instr) bool {
	if instruction.Op != ir.OCall || len(instruction.Args) != 3 {
		return false
	}
	callee := instruction.Arg(0)
	if callee.Kind != ir.RefConst {
		return false
	}
	constant := function.Consts[callee.ID]
	if constant.Kind != ir.ConstSym {
		return false
	}
	return function.Module().SymAttrOf(constant.Sym).Has(ir.SymAtomicPointerStore)
}

func derivedHeapBase(instruction ir.Instr, bases map[uint32]uint32) (uint32, bool) {
	if instruction.Op == ir.OCopy || instruction.Op == ir.OCast {
		return heapBase(instruction.Arg(0), bases)
	}
	if (instruction.Op != ir.OAdd && instruction.Op != ir.OSub) || instruction.Cls != ir.ClsP {
		return 0, false
	}
	if base, ok := heapBase(instruction.Arg(0), bases); ok {
		return base, true
	}
	if instruction.Op == ir.OAdd {
		if base, ok := heapBase(instruction.Arg(1), bases); ok {
			return base, true
		}
	}
	return 0, false
}

func heapBase(reference ir.Ref, bases map[uint32]uint32) (uint32, bool) {
	if reference.Kind != ir.RefTemp {
		return 0, false
	}
	base, ok := bases[reference.ID]
	return base, ok
}

func isTrackedHeapDerivation(instruction ir.Instr, bases map[uint32]uint32) bool {
	if instruction.To.Kind != ir.RefTemp {
		return false
	}
	_, ok := bases[instruction.To.ID]
	return ok && instruction.Op != ir.OHeapAlloc
}

func benignMemoryCall(function *ir.Func, instruction ir.Instr) bool {
	if instruction.Op != ir.OCall || len(instruction.Args) == 0 {
		return false
	}
	callee := instruction.Arg(0)
	if callee.Kind != ir.RefConst {
		return false
	}
	constant := function.Consts[callee.ID]
	if constant.Kind != ir.ConstSym {
		return false
	}
	switch constant.Sym {
	case "memset", "memcpy", "memmove", "memcmp",
		"goc_memset", "goc_memcpy", "goc_memmove", "goc_memcmp",
		"runtime.growslice":
		return true
	default:
		return false
	}
}

// memoryCallReadsOut names the operands of a benign memory call whose pointee is
// copied somewhere the call's own storage does not reach: the source of a block
// copy, and -- conservatively, since its argument order is not modelled here --
// every operand of a growslice.
//
// A memset writes and a memcmp compares, so neither can carry a pointer out of
// the object it was handed. A copy's destination cannot either: filling an
// object is how a container gets its contents in the first place, and treating
// that as a read-out would escape every payload goc puts into a variadic call.
func memoryCallReadsOut(function *ir.Func, instruction ir.Instr) []ir.Ref {
	if _, source, _, isCopy := memoryCopyOperands(function, instruction); isCopy {
		return []ir.Ref{source}
	}
	switch constSymbolName(function, instruction.Arg(0)) {
	case "memset", "goc_memset", "memcmp", "goc_memcmp":
		return nil
	case "memcpy", "memmove", "goc_memcpy", "goc_memmove":
		// A copy whose size is not a constant, so memoryCopyOperands did not
		// recognise it. The operand order is the same; only the size is unknown.
		if len(instruction.Args) == 4 {
			return []ir.Ref{instruction.Arg(2)}
		}
	}
	return instruction.Args[1:]
}

func memoryCopyOperands(function *ir.Func, instruction ir.Instr) (ir.Ref, ir.Ref, int64, bool) {
	if instruction.Op != ir.OCall || len(instruction.Args) != 4 {
		return ir.R, ir.R, 0, false
	}
	callee := instruction.Arg(0)
	if callee.Kind != ir.RefConst {
		return ir.R, ir.R, 0, false
	}
	constant := function.Consts[callee.ID]
	if constant.Kind != ir.ConstSym {
		return ir.R, ir.R, 0, false
	}
	switch constant.Sym {
	case "memcpy", "memmove", "goc_memcpy", "goc_memmove":
	default:
		return ir.R, ir.R, 0, false
	}
	size, knownSize := constInt(function, instruction.Arg(3))
	if !knownSize || size < 0 {
		return ir.R, ir.R, 0, false
	}
	return instruction.Arg(1), instruction.Arg(2), size, true
}
