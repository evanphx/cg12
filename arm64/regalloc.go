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
				var managed []int
				for _, temporary := range live.Members() {
					// A root is a live managed pointer, identified purely by value
					// via GCRef -- the register allocator needs no knowledge of the
					// calling convention. A copying-stack frontend (goc) marks every
					// pointer that must survive stack growth; a fixed-stack C/Ruby
					// frontend marks only its genuine heap references. A pointer that
					// is instead rematerialized (a frame address recomputed from the
					// runtime-updated frame pointer) is not live here and correctly
					// contributes no root.
					if f.Temps[temporary].GCRef {
						managed = append(managed, temporary)
					}
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
