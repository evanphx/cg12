package opt

import (
	"fmt"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// LoopAliasKind names how an allocation made inside a loop body is still
// reachable after the iteration that made it.
type LoopAliasKind string

const (
	// LoopAliasParked is the address written into frame storage that is not
	// re-created by the next iteration, so the next trip round the loop reads
	// this iteration's object out of it.
	LoopAliasParked LoopAliasKind = "parked"
	// LoopAliasCarried is the address handed round the loop's back edge in an
	// SSA value, which is what LoopAliasParked becomes once mem2reg has promoted
	// the slot it was parked in.
	LoopAliasCarried LoopAliasKind = "carried"
)

// LoopAliasDestination classifies what kept the address alive past its
// iteration. Like FrameEscapeDestination it names a category rather than an IR
// temporary, so that a baseline keyed on it does not churn when an unrelated
// change renumbers the function.
type LoopAliasDestination string

const (
	// LoopAliasIntoOuterSlot is a frame slot allocated outside the loop body --
	// a variable declared further out, which the next iteration does not
	// re-declare and so does not overwrite before reading.
	LoopAliasIntoOuterSlot LoopAliasDestination = "a frame slot the loop does not re-create"
	// LoopAliasIntoBackEdge is the loop's own back edge: a phi at the header
	// takes the address as its incoming value from a latch.
	LoopAliasIntoBackEdge LoopAliasDestination = "the loop's back edge"
)

// LoopAlias is one allocation that a loop body places in a frame slot, whose
// address is still reachable in the iteration after the one that created it.
//
// It is a statement about the emitted IR, not about the Go source. Go gives a
// variable declared in a loop body one instance per trip round the loop; a frame
// slot is one instance for the whole function. Where the difference is
// observable -- where two iterations' objects are live at once -- one slot makes
// two objects the source says are distinct into the same object.
type LoopAlias struct {
	// Func is the IR function the loop is in.
	Func string
	// Kind is how the address survives the iteration.
	Kind LoopAliasKind
	// Destination is the category of storage that kept it alive.
	Destination LoopAliasDestination
	// Pos is the source position of the allocation left in the frame, when the
	// front end recorded one.
	Pos ir.SrcPos
	// File is Pos's file name, resolved against the module's file table.
	File string
	// CrossPos is where the address crosses the iteration boundary: the store
	// that parked it, or the header of the loop whose back edge carries it.
	CrossPos ir.SrcPos
	// CrossFile is CrossPos's file name.
	CrossFile string
	// Alloc is the frame allocation, as an IR temporary name. It is diagnostic
	// detail, not part of Key.
	Alloc string
	// Slot is the temporary naming the storage that kept the address, for a
	// parked finding. Diagnostic detail, not part of Key.
	Slot string
}

// Key is the finding's stable identity: the two source positions that bound it
// -- where the object is allocated and where its address outlives the iteration
// -- plus the function and the shape. IR temporary names are deliberately
// excluded; they renumber whenever an unrelated change emits one more
// instruction.
func (alias LoopAlias) Key() string {
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s",
		location(alias.File, alias.Pos), alias.Func, alias.Kind, alias.Destination,
		location(alias.CrossFile, alias.CrossPos))
}

// String renders one finding as a single diagnostic line.
func (alias LoopAlias) String() string {
	held := string(alias.Destination)
	if alias.Slot != "" {
		held = fmt.Sprintf("%s (%s)", held, alias.Slot)
	}
	return fmt.Sprintf("%s: %s: %s is in a frame slot but its address is %s past the iteration, in %s at %s",
		location(alias.File, alias.Pos), alias.Func, alias.Alloc, alias.Kind, held,
		location(alias.CrossFile, alias.CrossPos))
}

// location renders a source position the way every audit in this package does.
func location(file string, pos ir.SrcPos) string {
	if !pos.Valid() {
		return "?"
	}
	if file == "" {
		file = "?"
	}
	return fmt.Sprintf("%s:%d:%d", file, pos.Line, pos.Col)
}

// LoopAliases reports every allocation in module that a loop body leaves in a
// frame slot while a pointer to that slot outlives the iteration that made it.
//
// # The defect this exists for
//
// Go gives a variable declared in a loop body one instance per trip round the
// loop. A frame slot is one instance for the whole call. Where the two disagree
// -- where an address made in iteration k is still reachable in iteration k+1 --
// the compiler has turned two objects the source says are distinct into one, and
//
//	var p, q *[2]int
//	for i := 0; i < n; i++ {
//		var a [2]int
//		a[0] = i
//		p, q = q, &a
//	}
//	return p[0], q[0]      // Go: 1 2. One slot: 2 2.
//
// prints a wrong answer with nothing failing anywhere. That was a live
// miscompile on this tree in four allocation forms, at both -O settings.
//
// # Why it needs its own audit
//
// FrameEscapes cannot see it, and cannot be extended to. It is a may-analysis
// over publications: it reports a frame address stored somewhere that outlives
// the frame. Here nothing is published. Both pointers stay inside the frame and
// are simply the same pointer, so there is no store for it to look at -- its
// destination test, isFrameAddress, is exactly what makes these stores
// invisible to it. The allocation census has the same problem from the other
// side: one frame slot per loop is what it expects to see, and it cannot tell a
// correct promotion from this one. Running the reductions and reading what they
// print is what caught the defect, and that instrument covers the three programs
// somebody thought to write.
//
// # The rule
//
// An allocation is reported when all of these hold:
//
//   - it is a frame allocation -- an OAlloc or OAllocN, the same predicate
//     FrameEscapes uses -- so it is one object, not one per iteration;
//   - the instruction making it is inside the body of a natural loop, so the
//     source expects one object per iteration;
//   - an address derived from it reaches storage the next iteration does not
//     re-create: a frame slot whose own allocation is outside that loop body, or
//     a phi at the loop header taking it in along a back edge.
//
// The third clause is the whole calibration. It is deliberately not "an
// allocation in a loop body", which would fire on every scratch buffer in every
// loop in the corpus; and deliberately not "an address the iteration does not
// consume", which needs a liveness argument this cannot make. It is the IR-level
// shadow of the rule the front end enforces at the source level
// (goc's findIterationCaptures): an address that reaches storage declared
// outside the loop is an address that outlives the iteration. Auditing against
// the same rule the fix implements is the point -- a finding is a place the fix
// did not reach.
//
// Storing into a slot that the loop body allocates is not a finding. Such a slot
// is a variable the source re-declares on every trip, so the front end emits its
// initialising store inside the loop too and the next iteration overwrites it
// before it can be read. It is the same one object in the frame either way, and
// unobservably so.
//
// A destination that is not this frame's own storage is not a finding either.
// An address of a loop-body allocation stored into a global, a heap object or
// memory reached through a parameter has left the frame altogether, which is
// FrameEscapes' finding and a worse one; reporting it here as well would put the
// same defect in two baselines.
//
// # What a clean run does not mean
//
// It cannot see:
//
//   - an aliasing that stays in SSA values with no phi at the header. A value
//     defined in the loop and used after it is not reported, and correctly so:
//     it names the final iteration's object, which one slot represents
//     faithfully. But a merge that is not the header's -- an irreducible loop,
//     or one whose latch phi was folded -- is missed rather than dismissed;
//   - a slot that holds more than one thing. The slot tracking is frameFacts',
//     which is first-write-wins: a slot written with some other frame
//     allocation before the loop-body one records only the first, so an address
//     read back out of it is attributed to the wrong allocation or to none;
//   - a slot reached at a run-time offset, for the same reason FrameEscapes
//     cannot: slots are keyed by an exactly known displacement, so an address
//     parked in frameArray[i] is neither recorded nor recognised;
//   - a destination reached through a pointer loaded from memory. It is not
//     provably this frame's storage, so it is left to FrameEscapes -- which
//     skips it too when its own slot tracking says the loaded pointer is a frame
//     address. Neither audit reports it;
//   - a bulk copy. Only stores are inspected, so a memcpy of a frame struct
//     holding a loop-body address into an outer slot carries it out unseen;
//   - a loop whose body the front end lowered into a separate function -- a
//     range-over-function body, or any loop inside a closure. The allocation and
//     the loop are then in different ir.Funcs and this is a per-function
//     analysis;
//   - whether the aliasing is observable. This is a may-analysis: it reports
//     that two iterations' objects can be the same object, not that any program
//     reads both. That is the same standard FrameEscapes holds itself to.
func LoopAliases(module *ir.Module) []LoopAlias {
	var aliases []LoopAlias
	for _, function := range module.Funcs {
		if function.Start == nil {
			continue
		}
		aliases = append(aliases, loopAliasesIn(module, function)...)
	}
	return aliases
}

// loopAllocations is the per-function structure the rule needs on top of
// frameFacts: which natural loop each frame allocation was made in, and which
// block each was made in, so a destination slot can be tested for membership of
// that loop's body.
type loopAllocations struct {
	// loop maps a frame allocation to the innermost natural loop containing it.
	// Only allocations inside a loop are present; the innermost is the right
	// one, since a slot outside it does not survive that loop's back edge
	// whatever the enclosing loops do.
	loop map[uint32]*analysis.Loop
	// block maps every frame allocation, in a loop or not, to the block that
	// makes it. Destinations are tested against this, and a destination may
	// perfectly well be a slot outside every loop.
	block map[uint32]*ir.Block
	// pos maps every frame allocation to the position of the making
	// instruction.
	pos map[uint32]ir.SrcPos
}

// effectivePositions gives every instruction in a function a source position:
// its own when it has one, and otherwise the last one seen before it in the same
// block, starting from the block's own.
//
// The front end emits positions the way QBE prints them -- a `loc` directive
// changes the current position and everything after it inherits -- so an
// instruction with no position of its own is not unlocated, it is at the last
// position named. Reading it that way matters here because both ends of a
// finding are source positions and its identity in the baseline is built from
// them: without the fallback, three separate loop aliases in one runtime
// function come out as `?` and collapse into one baseline line that no longer
// distinguishes them.
func effectivePositions(function *ir.Func) map[*ir.Instr]ir.SrcPos {
	positions := make(map[*ir.Instr]ir.SrcPos)
	for _, block := range function.Blocks {
		current := block.Pos
		if !current.Valid() {
			// A block the front end entered without naming, whose first
			// instructions are the address arithmetic for a statement it names a
			// few instructions later. Reading forward for the first position in
			// the block puts them at the statement they belong to, which is
			// better than the alternative: a `?` that every unnamed instruction
			// in the function shares, and that collapses distinct findings into
			// one baseline line.
			for index := range block.Instrs {
				if block.Instrs[index].Pos.Valid() {
					current = block.Instrs[index].Pos
					break
				}
			}
		}
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.Pos.Valid() {
				current = instruction.Pos
			}
			positions[instruction] = current
		}
	}
	return positions
}

func loopAliasesIn(module *ir.Module, function *ir.Func) []LoopAlias {
	cfg := analysis.BuildCFG(function)
	if len(cfg.RPO) == 0 {
		return nil
	}
	forest := cfg.LoopForest(cfg.Dominators())
	if len(forest.Loops) == 0 {
		return nil
	}

	positions := effectivePositions(function)
	allocations := &loopAllocations{
		loop:  make(map[uint32]*analysis.Loop),
		block: make(map[uint32]*ir.Block),
		pos:   make(map[uint32]ir.SrcPos),
	}
	for _, block := range function.Blocks {
		loop := forest.In[block]
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if !isFrameAllocation(*instruction) || instruction.To.Kind != ir.RefTemp {
				continue
			}
			allocations.block[instruction.To.ID] = block
			allocations.pos[instruction.To.ID] = positions[instruction]
			if loop != nil {
				allocations.loop[instruction.To.ID] = loop
			}
		}
	}
	if len(allocations.loop) == 0 {
		// Every frame allocation in this function is made once per call, so
		// there is nothing for one slot per iteration to be wrong about. This is
		// the common case and it skips the expensive part.
		return nil
	}

	facts := newFrameFacts(function)
	if len(facts.base) == 0 {
		return nil
	}

	var aliases []LoopAlias
	report := func(kind LoopAliasKind, destination LoopAliasDestination, allocation uint32, cross ir.SrcPos, slot string) {
		aliases = append(aliases, LoopAlias{
			Func:        function.Name,
			Kind:        kind,
			Destination: destination,
			Pos:         allocations.pos[allocation],
			File:        module.FileName(allocations.pos[allocation].File),
			CrossPos:    cross,
			CrossFile:   module.FileName(cross.File),
			Alloc:       facts.allocName(allocation),
			Slot:        slot,
		})
	}

	for _, block := range function.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if !instruction.Op.IsStore() {
				continue
			}
			allocation, derived := facts.frameBase(instruction.Arg(0))
			if !derived {
				continue
			}
			loop, made := allocations.loop[allocation]
			if !made {
				continue
			}
			if !loop.Body[block] {
				// The store is in the loop's exit region: it is reached from
				// inside an iteration but cannot reach a latch, so no further
				// iteration follows it and the address it parks is the last
				// iteration's -- which one slot represents faithfully. This is
				// not a technicality: runtime.getGCMaskOnDemand parks the
				// address of a loop-body local in a closure environment on the
				// path that returns, and reading that as loop-carried would be
				// wrong.
				continue
			}
			slot, outlives := outlivingSlot(facts, allocations, loop, instruction.Arg(1))
			if !outlives {
				continue
			}
			report(LoopAliasParked, LoopAliasIntoOuterSlot, allocation, positions[instruction], facts.tempName(slot))
		}
	}
	carriedAcrossBackEdges(facts, allocations, forest, report)
	return aliases
}

// carriedAcrossBackEdges reports the SSA half of the rule: a header phi whose
// incoming value along a back edge is the address of an allocation the loop body
// makes. That is the same defect as a parked address with the slot promoted
// away, and it is the form the audit would see if it ever ran after mem2reg.
func carriedAcrossBackEdges(
	facts *frameFacts,
	allocations *loopAllocations,
	forest *analysis.LoopForest,
	report func(LoopAliasKind, LoopAliasDestination, uint32, ir.SrcPos, string),
) {
	for _, loop := range forest.Loops {
		latches := make(map[*ir.Block]bool, len(loop.Latches))
		for _, latch := range loop.Latches {
			latches[latch] = true
		}
		for _, phi := range loop.Header.Phis {
			for index, argument := range phi.Args {
				if index >= len(phi.Blocks) || !latches[phi.Blocks[index]] {
					continue
				}
				allocation, derived := facts.frameBase(argument)
				if !derived {
					continue
				}
				// The allocation has to be made by this loop for its instances to
				// be per-iteration of it. One made further out is a single object
				// the loop merely passes around, which is what the source says
				// too.
				if made, known := allocations.block[allocation]; !known || !loop.Body[made] {
					continue
				}
				report(LoopAliasCarried, LoopAliasIntoBackEdge, allocation, blockPosition(loop.Header), "")
			}
		}
	}
}

// outlivingSlot reports whether a store destination is frame storage the given
// loop does not re-create, and names the allocation behind it.
//
// Three answers are possible and only one is a finding. Storage the loop body
// allocates is re-declared by the next iteration and is not one. Storage that is
// not this frame's at all -- a global, a heap object, memory reached through a
// parameter -- is not one either: that address has left the frame, which is
// FrameEscapes' report to make. What is left is a frame slot allocated outside
// the loop, which the next iteration reads without having written.
func outlivingSlot(facts *frameFacts, allocations *loopAllocations, loop *analysis.Loop, destination ir.Ref) (uint32, bool) {
	var outliving uint32
	found := false
	facts.eachFrameSlotBase(destination, func(base uint32) {
		if found {
			return
		}
		made, known := allocations.block[base]
		if !known || loop.Body[made] {
			return
		}
		outliving, found = base, true
	})
	return outliving, found
}

// eachFrameSlotBase calls visit with the allocation behind every frame slot a
// pointer may name.
//
// It is walkFrameSlots with the offset dropped. This audit asks which allocation
// the destination belongs to, not which bytes of it are written: a slot outside
// the loop outlives the iteration at every displacement, including the run-time
// ones walkFrameSlots declines to name.
func (facts *frameFacts) eachFrameSlotBase(reference ir.Ref, visit func(uint32)) {
	facts.walkFrameSlotBases(reference, visit, nil)
}

func (facts *frameFacts) walkFrameSlotBases(reference ir.Ref, visit func(uint32), visiting map[uint32]bool) {
	base, _ := facts.stripToBase(reference)

	location := facts.aliases.locOf(base, 1)
	if location.keyKind == keyLocal || location.keyKind == keyEscaped {
		visit(uint32(location.keyID))
		return
	}

	arguments := facts.mergeArguments(base)
	if len(arguments) == 0 {
		return
	}
	if visiting == nil {
		visiting = make(map[uint32]bool)
	}
	if visiting[base.ID] {
		return
	}
	visiting[base.ID] = true
	defer delete(visiting, base.ID)
	for _, argument := range arguments {
		facts.walkFrameSlotBases(argument, visit, visiting)
	}
}
