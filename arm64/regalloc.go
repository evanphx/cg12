package arm64

import (
	"fmt"
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
}

// numbering assigns each instruction (and block boundary) a position. Positions
// increase in reverse-postorder so live-range endpoints compare sensibly.
type numbering struct {
	pos      map[*ir.Block][2]int // [firstPos, lastPos] of the block body
	callAt   []int                // positions holding a call
	safeAt   []int                // positions that are safepoints (calls + OSafepoint)
	posInstr []*ir.Instr          // position -> instruction (nil at terminator slots)
	order    []*ir.Block
	next     int
}

// regAlloc runs linear-scan allocation on the already-lowered function.
func regAlloc(f *ir.Func) (*allocation, error) {
	if err := asmPrecolor(f); err != nil {
		return nil, err
	}
	cfg := analysis.BuildCFG(f)
	live := cfg.Liveness()

	num := numberInstrs(cfg)
	intervals := buildIntervals(f, cfg, live, num)

	alloc, err := linearScan(f, intervals, num)
	if err != nil {
		return nil, err
	}
	alloc.intervals = intervals
	alloc.posInstr = num.posInstr
	alloc.safeRoots = computeSafepointRoots(f, intervals, num)
	return alloc, nil
}

// computeSafepointRoots returns, for each safepoint (a call or an explicit
// OSafepoint), the managed-reference temporaries live across it (defined before,
// used after) — the GC roots to report there. For a call, its own arguments
// (dead at the call) and result (not yet defined) are naturally excluded; for an
// OSafepoint, which neither defines nor uses anything, every live reference is a
// root.
func computeSafepointRoots(f *ir.Func, intervals []*interval, num *numbering) map[*ir.Instr][]int {
	roots := map[*ir.Instr][]int{}
	for _, p := range num.safeAt {
		in := num.posInstr[p]
		if in == nil {
			continue
		}
		var live []int
		for _, iv := range intervals {
			if iv.start < p && p < iv.end && f.Temps[iv.temp].GCRef {
				live = append(live, iv.temp)
			}
		}
		sort.Ints(live)
		roots[in] = live
	}
	return roots
}

// numberInstrs lays instructions out in RPO and assigns each a position, noting
// which positions are calls and which instruction occupies each position.
func numberInstrs(cfg *analysis.CFG) *numbering {
	n := &numbering{pos: map[*ir.Block][2]int{}, order: cfg.RPO}
	for _, b := range cfg.RPO {
		first := n.next
		for k := range b.Instrs {
			switch b.Instrs[k].Op {
			case ir.OCall:
				n.callAt = append(n.callAt, n.next)
				n.safeAt = append(n.safeAt, n.next)
			case ir.OAsm:
				n.callAt = append(n.callAt, n.next) // clobbers like a call, but is not a GC safepoint
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

	for _, b := range cfg.RPO {
		bp := num.pos[b]
		// Live-in temps are live from the block's first position...
		for _, id := range live.LiveIn(b).Members() {
			extend(id, bp[0])
		}
		// ...live-out temps through the terminator position.
		for _, id := range live.LiveOut(b).Members() {
			extend(id, bp[1])
		}
		p := bp[0]
		for k := range b.Instrs {
			in := &b.Instrs[k]
			// Inline-asm inputs are early-clobber against the outputs: extending
			// them one position past the instruction keeps them live through the
			// output definitions, so the allocator never reuses an input register
			// for an output (which would corrupt an input still read by the asm).
			argEnd := p
			if in.Op == ir.OAsm {
				argEnd = p + 1
			}
			for _, a := range in.Args {
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
				break
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

// linearScan assigns registers to intervals in start order, spilling to the
// stack when no suitable register is free.
func linearScan(f *ir.Func, intervals []*interval, num *numbering) (*allocation, error) {
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].start != intervals[j].start {
			return intervals[i].start < intervals[j].start
		}
		return intervals[i].end < intervals[j].end
	})

	alloc := &allocation{}
	active := []*interval{} // sorted by end
	inUse := map[Reg]bool{}
	slotFor := map[int]int{}

	freeExpired := func(start int) {
		kept := active[:0]
		for _, a := range active {
			if a.end < start {
				if r := Reg(f.Temps[a.temp].Reg); f.Temps[a.temp].Reg != ir.NoReg {
					delete(inUse, r)
				}
			} else {
				kept = append(kept, a)
			}
		}
		active = kept
	}
	addActive := func(iv *interval) {
		active = append(active, iv)
		sort.Slice(active, func(i, j int) bool { return active[i].end < active[j].end })
	}
	spill := func(iv *interval) {
		t := f.Temps[iv.temp]
		if _, ok := slotFor[iv.temp]; !ok {
			slotFor[iv.temp] = alloc.spillBytes
			alloc.spillBytes += 8
		}
		t.Slot = slotFor[iv.temp]
		t.Reg = ir.NoReg
	}

	for _, iv := range intervals {
		freeExpired(iv.start)

		// Pre-coloured intervals must take their register; evict any conflict.
		if iv.precolor >= 0 {
			r := iv.precolor
			if inUse[r] {
				evictFrom(f, active, r, spill)
				delete(inUse, r)
			}
			f.Temps[iv.temp].Reg = int(r)
			inUse[r] = true
			addActive(iv)
			continue
		}

		// A managed reference live across a safepoint is kept in a stack slot, so
		// the GC finds every root at a frame offset — no callee-saved register
		// tracking needed. (We don't split live ranges, so it stays spilled for
		// its whole life.)
		if iv.crossSafe && f.Temps[iv.temp].GCRef {
			spill(iv)
			continue
		}

		if r, ok := pickRegister(iv, inUse); ok {
			f.Temps[iv.temp].Reg = int(r)
			inUse[r] = true
			addActive(iv)
		} else {
			spill(iv)
		}
	}

	// Rewrite each spilled temp's binding is already recorded on ir.Temp; the
	// emitter reads Temp.Slot / Temp.Reg directly.
	if err := verifyNoSpillOfPrecolor(f, intervals); err != nil {
		return nil, err
	}
	return alloc, nil
}

// pickRegister chooses a free register for iv from the pool matching its class,
// honouring the call-crossing constraint (such intervals must land in
// callee-saved registers).
func pickRegister(iv *interval, inUse map[Reg]bool) (Reg, bool) {
	order := intAllocOrder
	if iv.float {
		order = floatAllocOrder
	}
	for _, r := range order {
		if inUse[r] {
			continue
		}
		if iv.crosses && !calleeSavedReg(r) {
			continue
		}
		return r, true
	}
	return 0, false
}

// evictFrom spills whichever active interval currently holds register r.
func evictFrom(f *ir.Func, active []*interval, r Reg, spill func(*interval)) {
	for _, a := range active {
		if a.precolor < 0 && f.Temps[a.temp].Reg == int(r) {
			spill(a)
			return
		}
	}
}

func verifyNoSpillOfPrecolor(f *ir.Func, intervals []*interval) error {
	for _, iv := range intervals {
		if iv.precolor >= 0 && f.Temps[iv.temp].Reg != int(iv.precolor) {
			return fmt.Errorf("arm64: could not honour pre-coloured register for temp %%%s", f.Temps[iv.temp].Name)
		}
	}
	return nil
}
