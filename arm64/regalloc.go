package arm64

import (
	"os"
	"sort"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// allocation is the result of register allocation: every temporary is bound to
// either a physical register or a spill slot, and the number of stack bytes the
// frame must reserve for spills is recorded.
type allocation struct {
	spillBytes int
	// Each temp's binding lives on the ir.Temp itself (Reg / Slot).

	// Liveness substrate, shared by DWARF variable locations and GC stack maps.
	intervals []*interval         // per-temp live ranges in the numbering
	posInstr  []*ir.Instr         // numbering position -> instruction (nil at block ends)
	safeRoots map[*ir.Instr][]int // safepoint (call or OSafepoint) -> live GC-ref temps

	// remat maps a spilled-but-rematerialisable temp to the rule for recomputing it
	// at each use (it has no spill slot).
	remat map[int]rematRule
}

// interval is a temporary's live range over a linear instruction numbering,
// conservatively covering any holes (always correct, occasionally wasteful).
type interval struct {
	temp      int
	start     int
	end       int
	crosses   bool // live across at least one call
	crossSafe bool // live across at least one safepoint (call or OSafepoint)
	precolor  Reg  // fixed register, or -1
	float     bool // temp is a floating-point value (allocate from V registers)

	// avoid holds registers this interval may not use because an inline asm it
	// is live across writes them. Being callee-saved is no protection: a clobber
	// list is exactly how an asm says it writes one.
	avoid map[Reg]bool
}

// numbering assigns each instruction (and block boundary) a position. Positions
// increase in reverse-postorder so live-range endpoints compare sensibly.
type numbering struct {
	pos    map[*ir.Block][2]int // [firstPos, lastPos] of the block body
	callAt []int                // positions holding a call
	// asmClobbers maps an OAsm's position to the registers its template writes
	// beyond its outputs. Being live across one of these rules those registers out,
	// which "crosses a call" does not say: that only rules out the caller-saved set.
	asmClobbers map[int][]Reg
	safeAt      []int       // positions that are safepoints (calls + OSafepoint)
	posInstr    []*ir.Instr // position -> instruction (nil at terminator slots)
	order       []*ir.Block
	next        int
}

// regAlloc runs graph-colouring allocation on the already-lowered function.
//
// The colouring engine (colorAlloc) decides each temp's register or spill slot; the
// conservative per-temp intervals are still built here, but only as the liveness
// substrate that DWARF variable locations and GC stack maps read.
func regAlloc(f *ir.Func) (*allocation, error) {
	if os.Getenv("CG12_DUMP_IR") == f.Name {
		os.Stderr.WriteString(f.String())
	}
	if err := asmPrecolor(f); err != nil {
		return nil, err
	}
	cfg := analysis.BuildCFG(f)
	live := cfg.Liveness()
	freq := cfg.Frequency(cfg.Dominators())

	alloc, err := colorAlloc(f, cfg, live, freq)
	if err != nil {
		return nil, err
	}

	// Colouring may place a call-crossing value in a caller-saved register; save and
	// restore it around each call it crosses. This inserts instructions into the
	// blocks, so the liveness substrate (which holds *ir.Instr pointers) is built
	// afterward, on the final instruction list.
	if err := insertCallerSaves(f, cfg, live, alloc); err != nil {
		return nil, err
	}
	// insertCallerSaves rebuilds block instruction slices, relocating the OAlloc
	// instructions a rematerialised alloca-address rule points at; re-resolve those
	// pointers so computeFrame's allocOff (keyed by the final instruction) matches.
	refreshRematAllocas(f, alloc.remat)

	cfg = analysis.BuildCFG(f)
	live = cfg.Liveness()
	num := numberInstrs(cfg)
	alloc.intervals = buildIntervals(f, cfg, live, num)
	alloc.posInstr = num.posInstr
	alloc.safeRoots = computeSafepointRoots(f, cfg, live)
	maybeDumpRanges(f, alloc, num, cfg, freq)
	dumpPressureReport(f, alloc, num, cfg, live, freq)
	return alloc, nil
}

// computeSafepointRoots returns, for each safepoint (a call or an explicit
// OSafepoint), the managed-reference temporaries live across it (defined before,
// used after) — the GC roots to report there. For a call, its own arguments
// (dead at the call) and result (not yet defined) are naturally excluded; for an
// OSafepoint, which neither defines nor uses anything, every live reference is a
// root.
func computeSafepointRoots(f *ir.Func, cfg *analysis.CFG, liveness *analysis.Liveness) map[*ir.Instr][]int {
	roots := map[*ir.Instr][]int{}
	allocationsOf := pointerAllocationSources(f)
	escaping := frameEscapingAllocations(f, allocationsOf)
	for _, block := range cfg.RPO {
		live := liveness.LiveOut(block).Copy()
		live.AddRef(block.Jmp.Arg)
		for _, argument := range block.Jmp.Args {
			live.AddRef(argument)
		}

		for index := len(block.Instrs) - 1; index >= 0; index-- {
			instruction := &block.Instrs[index]

			// A result does not exist until after its defining instruction, so it
			// cannot be a root while a call is executing. Remove definitions before
			// recording the values which are live across this instruction.
			if instruction.To.IsTemp() {
				live.Remove(int(instruction.To.ID))
			}
			for _, definition := range instruction.Defs {
				if definition.IsTemp() {
					live.Remove(int(definition.ID))
				}
			}

			if instruction.Op == ir.OCall || instruction.Op == ir.OSafepoint {
				recorded := map[int]bool{}
				for allocation := range escaping {
					recorded[allocation] = true
				}
				for _, temporary := range live.Members() {
					if isSafepointRoot(f, temporary) {
						recorded[temporary] = true
					}
					for _, allocation := range allocationsOf[temporary] {
						recorded[allocation] = true
					}
				}
				managed := make([]int, 0, len(recorded))
				for temporary := range recorded {
					managed = append(managed, temporary)
				}
				sort.Ints(managed)
				roots[instruction] = managed
			}

			// Arguments become live before the instruction. Recording roots first
			// excludes call-only arguments, while retaining an argument that also
			// has a genuine use after the call.
			for _, argument := range instruction.Uses() {
				live.AddRef(argument)
			}
		}
	}
	return roots
}

// isSafepointRoot reports whether a live temporary has to be described at a
// safepoint.
//
// A root is a live managed pointer, identified purely by value via GCRef -- the
// register allocator needs no knowledge of the calling convention. A
// copying-stack frontend (goc) marks every pointer that must survive stack
// growth; a fixed-stack C/Ruby frontend marks only its genuine heap references.
// A pointer that is instead rematerialized (a frame address recomputed from the
// runtime-updated frame pointer) is not live here and correctly contributes no
// root.
//
// A managed frame adds a second class: any live pointer-class temporary, managed
// or not. Its spill slot can hold an interior stack address -- the address of a
// local -- and the runtime has to relocate that word when it copies the stack,
// which it only does for words the frame's map marks at the safepoint the copy
// unwinds through. Before per-safepoint maps became precise these words were
// covered by the function-wide conservative map instead; describing them per
// safepoint is what lets that map stop being unioned into every safepoint. A
// fixed C/Ruby stack never moves, so there raw pointers stay out of the root set
// and remain freely optimizable.
func isSafepointRoot(f *ir.Func, temporary int) bool {
	if f.Temps[temporary].GCRef {
		return true
	}
	return f.UsesManagedFrame() && f.Temps[temporary].Cls == ir.ClsP
}

// allocationAddresses maps a temporary to every pointer-bearing stack
// allocation it may hold an address into, in ascending temporary order.
type allocationAddresses map[int][]int

// pointerAllocationSources maps every temporary that may be a constant offset
// from a pointer-bearing stack allocation to those allocations' own temporaries.
//
// cg12 has no notion of a source-level variable, so it approximates the liveness
// of an address-taken local by the liveness of the temporary holding the
// allocation's address. That is what lets a safepoint report only the locals
// still in use. The approximation breaks when a derived address -- `&local.field`
// computed once and then reused -- outlives the base temporary: the local's
// pointer words would go unscanned while code can still reach them through the
// derived address. Reporting the allocation wherever any address into it is live
// restores the property the approximation needs.
//
// A temporary maps to a set rather than to one allocation because control flow
// merges addresses: `p := &a; if c { p = &b }`, and every interface argument goc
// builds, whose nil check selects between the value's descriptor and a zeroed
// one. A single mapping cannot express the merge, so such an allocation used to
// fall back to whole-frame conservatism. Reporting every allocation the merged
// temporary may address keeps the invariant with none of that loss.
//
// The result is always a subset of what the function-wide conservative map
// described, so it can never introduce a word the collector should not read.
// Only pointer-bearing allocations on a managed frame matter; elsewhere the map
// is empty.
func pointerAllocationSources(f *ir.Func) allocationAddresses {
	if !f.UsesManagedFrame() || len(f.StackPointerWords) == 0 {
		return nil
	}

	allocations := make(map[uint32]bool)
	// dependents[base] lists the temporaries that carry base's addresses onward.
	dependents := make(map[uint32][]uint32)
	carry := func(base ir.Ref, result ir.Ref) {
		if base.Kind != ir.RefTemp || result.Kind != ir.RefTemp {
			return
		}
		dependents[base.ID] = append(dependents[base.ID], result.ID)
	}

	for _, block := range f.Blocks {
		// Phis are destructed into copies before register allocation, so this loop
		// is normally empty. Following them anyway keeps the analysis independent
		// of where in the pipeline it runs.
		for _, phi := range block.Phis {
			for _, argument := range phi.Args {
				carry(argument, phi.To)
			}
		}
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.To.Kind != ir.RefTemp {
				continue
			}
			if instruction.Op.IsAlloc() {
				if len(f.StackPointerWords[instruction.To.ID]) > 0 {
					allocations[instruction.To.ID] = true
				}
				continue
			}
			for _, base := range addressDerivationBases(f, instruction) {
				carry(base, instruction.To)
			}
		}
	}

	reaching := make(map[uint32]map[uint32]bool, len(allocations))
	pending := make([]uint32, 0, len(allocations))
	for allocation := range allocations {
		reaching[allocation] = map[uint32]bool{allocation: true}
		pending = append(pending, allocation)
	}
	// Each visit can only add allocations to a temporary's set, and both sets are
	// finite, so the worklist drains.
	for next := 0; next < len(pending); next++ {
		base := pending[next]
		for _, result := range dependents[base] {
			target, known := reaching[result]
			if !known {
				target = map[uint32]bool{}
				reaching[result] = target
			}
			grew := false
			for allocation := range reaching[base] {
				if !target[allocation] {
					target[allocation] = true
					grew = true
				}
			}
			if grew {
				pending = append(pending, result)
			}
		}
	}

	sources := make(allocationAddresses, len(reaching))
	for temporary, set := range reaching {
		addressed := make([]int, 0, len(set))
		for allocation := range set {
			addressed = append(addressed, int(allocation))
		}
		sort.Ints(addressed)
		sources[int(temporary)] = addressed
	}
	return sources
}

// addressDerivationBases returns the operands whose addresses instruction's
// result may still name: a copy or constant-offset addition passes its base
// through, and a conditional select passes both of its value operands through.
// Anything else either produces a value that is not an address into the operand
// (a comparison result) or one this analysis cannot follow, and contributes no
// derivation.
func addressDerivationBases(f *ir.Func, instruction *ir.Instr) []ir.Ref {
	switch instruction.Op {
	case ir.OCopy, ir.OAdd:
		base, derived := constantOffsetBase(f, instruction)
		if !derived {
			return nil
		}
		return []ir.Ref{{Kind: ir.RefTemp, ID: base}}
	case ir.OSel:
		if len(instruction.Args) != 3 {
			return nil
		}
		return instruction.Args[1:3]
	default:
		return nil
	}
}

// frameEscapingAllocations returns the pointer-bearing stack allocations whose
// address is used for anything beyond reading and writing the allocation itself.
//
// Per-safepoint liveness is only sound for a local while every use of its
// address is visible in this frame. Once the address leaves -- passed to a call,
// or written into memory, as runtime.gopanic does when it publishes &p through
// g._panic, and as a caller does when it takes &pinner -- the frame can no
// longer tell when the local stops being read, and neither the collector nor
// stack copying may stop tracking it. Such an allocation is therefore reported
// at every safepoint for the whole life of the frame, which is what the
// function-wide conservative map used to do for every local.
//
// The test is deliberately syntactic and errs towards escaping: a use this
// function does not recognize as an address operand makes every allocation the
// operand may address conservative, which is never wrong, only less precise.
func frameEscapingAllocations(f *ir.Func, allocationsOf allocationAddresses) map[int]bool {
	if len(allocationsOf) == 0 {
		return nil
	}
	escaping := make(map[int]bool)
	note := func(use ir.Ref, addressOnly bool) {
		if use.Kind != ir.RefTemp || addressOnly {
			return
		}
		for _, allocation := range allocationsOf[int(use.ID)] {
			escaping[allocation] = true
		}
	}

	// A phi's arguments need no test: pointerAllocationSources carries each one
	// into the phi's result, so the address is still tracked on the other side.
	for _, block := range f.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			for argument, use := range instruction.Args {
				note(use, addressOnlyOperand(f, instruction, argument))
			}
			// A closure environment is read outside Args, and handing one to a
			// callee publishes it exactly as an ordinary operand would.
			note(instruction.ClosureContext, false)
		}
		note(block.Jmp.Arg, addressOnlyTerminatorOperand(block.Jmp.Kind))
		for _, argument := range block.Jmp.Args {
			note(argument, false)
		}
	}
	return escaping
}

// addressOnlyTerminatorOperand reports whether a terminator consumes its operand
// without letting it out of the frame. A conditional branch and a switch test
// their operand and produce no value; a return hands it to the caller, and a
// computed goto jumps to it, so both are escapes.
func addressOnlyTerminatorOperand(kind ir.JmpKind) bool {
	return kind == ir.JmpJnz || kind == ir.JmpSwitch
}

// addressOnlyOperand reports whether operand argument of instruction reads an
// address purely to address the allocation it names.
//
// A load addresses through every operand it has; a store does so through every
// operand but the value it writes; a block copy addresses through both of its
// operands. A lifetime marker only brackets the slot. A comparison yields a
// boolean, so no address leaves through it -- which is what an interface
// argument's nil check does with the descriptor it is about to pass. A copy, a
// constant addition and a conditional select derive a new address, tracked
// alongside the operand by pointerAllocationSources; an addition by a value that
// is not a constant is not, because its result need not stay inside the
// allocation.
func addressOnlyOperand(f *ir.Func, instruction *ir.Instr, argument int) bool {
	switch {
	case instruction.Op.IsLoad():
		return true
	case instruction.Op.IsStore():
		return argument != 0
	case instruction.Op.IsLifetime():
		return true
	case instruction.Op == ir.OBlit:
		return true
	case instruction.Op == ir.OCmp || instruction.Op == ir.OCCmp:
		return true
	case instruction.Op == ir.OCopy || instruction.Op == ir.OAdd:
		if instruction.To.Kind != ir.RefTemp {
			return false
		}
		_, derived := constantOffsetBase(f, instruction)
		return derived
	case instruction.Op == ir.OSel:
		return instruction.To.Kind == ir.RefTemp && len(instruction.Args) == 3
	case instruction.Op == ir.OCall:
		return memoryPrimitiveAddressOperand(f, instruction, argument)
	default:
		return false
	}
}

// memoryPrimitiveAddressOperand reports whether an operand of a call is an
// address handed to one of cg12's own memory primitives.
//
// goc lowers aggregate copies, zeroing and pointer stores to these three
// helpers, so a local aggregate is addressed through a call far more often than
// through a load or a store. Their semantics are fixed by the compiler that
// emits them: each reads or writes the memory it is given and retains no
// address, exactly like the load and store operands above. Treating them as
// escapes would make every aggregate local conservative and leave the
// over-retention of RUNTIME_PLAN.md 5.3 in place. Every other callee is an
// escape, including any runtime function, because nothing here knows what it
// does with the address.
func memoryPrimitiveAddressOperand(f *ir.Func, instruction *ir.Instr, argument int) bool {
	callee := instruction.Args[0]
	if callee.Kind != ir.RefConst {
		return false
	}
	constant := f.Consts[callee.ID]
	if constant.Kind != ir.ConstSym {
		return false
	}
	switch constant.Sym {
	case "goc_memcpy":
		// (destination, source, length)
		return argument == 1 || argument == 2
	case "goc_memset":
		// (destination, value, length)
		return argument == 1
	case "goc_storep":
		// (address, value): the value is a stored pointer, not an address.
		return argument == 1
	default:
		return false
	}
}

// constantOffsetBase returns the temporary an address is a fixed offset from,
// following copies and additions of an integer constant.
func constantOffsetBase(f *ir.Func, instruction *ir.Instr) (uint32, bool) {
	switch instruction.Op {
	case ir.OCopy:
		if len(instruction.Args) != 1 || instruction.Args[0].Kind != ir.RefTemp {
			return 0, false
		}
		return instruction.Args[0].ID, true

	case ir.OAdd:
		if len(instruction.Args) != 2 || instruction.Args[0].Kind != ir.RefTemp || instruction.Args[1].Kind != ir.RefConst {
			return 0, false
		}
		if f.Consts[instruction.Args[1].ID].Kind != ir.ConstInt {
			return 0, false
		}
		return instruction.Args[0].ID, true

	default:
		return 0, false
	}
}

// numberInstrs lays instructions out in reachable RPO followed by any
// unreachable synthetic blocks that still need code emission, noting which
// positions are calls and which instruction occupies each position.
func numberInstrs(cfg *analysis.CFG) *numbering {
	n := &numbering{pos: map[*ir.Block][2]int{}, order: allocationBlockOrder(cfg)}
	for _, b := range n.order {
		first := n.next
		for k := range b.Instrs {
			switch b.Instrs[k].Op {
			case ir.OCall:
				n.callAt = append(n.callAt, n.next)
				n.safeAt = append(n.safeAt, n.next)
			case ir.OAsm:
				n.callAt = append(n.callAt, n.next) // clobbers like a call, but is not a GC safepoint
				if regs := asmClobberRegs(&b.Instrs[k]); len(regs) > 0 {
					if n.asmClobbers == nil {
						n.asmClobbers = map[int][]Reg{}
					}
					n.asmClobbers[n.next] = regs
				}
			case ir.OSafepoint:
				n.safeAt = append(n.safeAt, n.next)
			}
			n.posInstr = append(n.posInstr, &b.Instrs[k])
			n.next++
		}
		n.posInstr = append(n.posInstr, nil) // terminator slot
		n.next++
		n.pos[b] = [2]int{first, n.next - 1}
	}
	return n
}

func allocationBlockOrder(cfg *analysis.CFG) []*ir.Block {
	order := make([]*ir.Block, 0, len(cfg.Fn.Blocks))
	seen := make(map[*ir.Block]bool, len(cfg.Fn.Blocks))
	for _, block := range cfg.RPO {
		order = append(order, block)
		seen[block] = true
	}
	for _, block := range cfg.Fn.Blocks {
		if seen[block] {
			continue
		}
		order = append(order, block)
	}
	return order
}

// buildIntervals derives one conservative live interval per temporary from block
// liveness plus the precise def/use positions inside blocks.
func buildIntervals(f *ir.Func, cfg *analysis.CFG, live *analysis.Liveness, num *numbering) []*interval {
	const inf = 1 << 30
	ivs := make([]*interval, len(f.Temps))
	for i := range ivs {
		ivs[i] = &interval{temp: i, start: inf, end: -1, precolor: -1, float: f.Temps[i].Cls.IsFloat()}
		if t := f.Temps[i]; t.Fixed {
			ivs[i].precolor = Reg(t.Reg)
		}
	}
	extend := func(id, p int) {
		iv := ivs[id]
		if p < iv.start {
			iv.start = p
		}
		if p > iv.end {
			iv.end = p
		}
	}

	for _, b := range num.order {
		bp := num.pos[b]
		// Live-in temps are live from the block's first position...
		if cfg.Reachable(b) {
			for _, id := range live.LiveIn(b).Members() {
				extend(id, bp[0])
			}
			// ...live-out temps through the terminator position.
			for _, id := range live.LiveOut(b).Members() {
				extend(id, bp[1])
			}
		}
		p := bp[0]
		for k := range b.Instrs {
			in := &b.Instrs[k]
			// A lifetime marker references its alloca to bound the slot's region, not
			// to read the value; extending the interval to it would misreport the
			// address temp as live there (matching instrUses / analysis.Liveness).
			if in.Op.IsLifetime() {
				p++
				continue
			}
			// Inline-asm inputs are early-clobber against the outputs: extending
			// them one position past the instruction keeps them live through the
			// output definitions, so the allocator never reuses an input register
			// for an output (which would corrupt an input still read by the asm).
			argEnd := p
			if in.Op == ir.OAsm {
				argEnd = p + 1
			}
			for _, a := range in.Uses() {
				if a.Kind == ir.RefTemp {
					extend(int(a.ID), argEnd)
				}
			}
			if in.To.Kind == ir.RefTemp {
				extend(int(in.To.ID), p)
			}
			for _, d := range in.Defs {
				if d.Kind == ir.RefTemp {
					extend(int(d.ID), p)
				}
			}
			p++
		}
		if b.Jmp.Arg.Kind == ir.RefTemp {
			extend(int(b.Jmp.Arg.ID), bp[1])
		}
		for _, a := range b.Jmp.Args {
			if a.Kind == ir.RefTemp {
				extend(int(a.ID), bp[1])
			}
		}
	}

	// A precoloured temp that is never defined (an incoming parameter register)
	// is live from function entry.
	for _, iv := range ivs {
		if iv.precolor >= 0 && iv.end < 0 {
			continue // completely unused precolour
		}
		if iv.precolor >= 0 && iv.start == inf {
			iv.start = 0
		}
	}

	// Mark intervals that span a call, and (matching computeSafepointRoots) those
	// live across any safepoint.
	var out []*interval
	for _, iv := range ivs {
		if iv.end < 0 {
			continue // temp never used
		}
		for _, c := range num.callAt {
			if iv.start <= c && c < iv.end {
				iv.crosses = true
				for _, r := range num.asmClobbers[c] {
					if iv.avoid == nil {
						iv.avoid = map[Reg]bool{}
					}
					iv.avoid[r] = true
				}
			}
		}
		for _, p := range num.safeAt {
			if iv.start < p && p < iv.end {
				iv.crossSafe = true
				break
			}
		}
		out = append(out, iv)
	}
	return out
}
