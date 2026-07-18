package interp

import (
	"math"
	"sort"

	"github.com/evanphx/cg12/ir"
)

// Register-window assignment. Parameters are pinned to [0,P) because the rolling
// window makes a callee's parameters its caller's staged arguments. Everything
// else -- non-parameter temporaries and the constants loaded in the prologue --
// is packed above P by liveness-based linear scan: two values whose live ranges
// do not overlap share a slot, so the window is as small as Lua's, not one slot
// per value. The outgoing-argument staging region and cycle-break scratch sit on
// top, above every packed local, so a callee (whose window rolls up to the
// staging region) never overwrites a value live across the call.
func (c *compiler) assignRegisters() error {
	next := uint16(0)
	for i, p := range c.f.Params {
		c.isParam[p.ID] = true
		c.reg[p.ID] = uint16(i)
		next++
	}
	if err := c.buildConstPool(); err != nil {
		return err
	}
	top, err := c.packWindow(next)
	if err != nil {
		return err
	}
	c.localTop = top
	c.argBase = top
	c.maxArgs = c.countMaxArgs()
	c.scratch = top + uint16(c.maxArgs)
	c.frameTop = c.scratch
	return nil
}

// buildConstPool materializes every constant used as a value operand, a phi
// argument, or a call argument into the const pool. Slots are assigned later by
// the packer.
func (c *compiler) buildConstPool() error {
	use := func(r ir.Ref) error {
		if r.Kind != ir.RefConst {
			return nil
		}
		if _, ok := c.constPool[r.ID]; ok {
			return nil
		}
		v, err := c.constValue(r.ID)
		if err != nil {
			return err
		}
		c.constPool[r.ID] = uint32(len(c.consts))
		c.consts = append(c.consts, v)
		return nil
	}
	return c.forEachOperand(use)
}

// forEachOperand visits every value operand: phi arguments, instruction operands
// (skipping a call's callee symbol), and terminator arguments.
func (c *compiler) forEachOperand(fn func(ir.Ref) error) error {
	for _, b := range c.f.Blocks {
		for _, p := range b.Phis {
			for _, a := range p.Args {
				if err := fn(a); err != nil {
					return err
				}
			}
		}
		for i := range b.Instrs {
			in := &b.Instrs[i]
			start := 0
			if in.Op == ir.OCall {
				start = 1
			}
			for _, a := range in.Args[start:] {
				if err := fn(a); err != nil {
					return err
				}
			}
		}
		if err := fn(b.Jmp.Arg); err != nil {
			return err
		}
		for _, a := range b.Jmp.Args {
			if err := fn(a); err != nil {
				return err
			}
		}
	}
	return nil
}

// liveRange is one value's conservative live interval over the instruction
// numbering: [start, end], holes filled. A temp and a const are packed the same
// way; only where the assigned slot is written differs.
type liveRange struct {
	start, end int
	isConst    bool
	id         uint32
}

// packWindow numbers the instructions, derives a live interval per non-parameter
// temporary and per constant, and linear-scans them into slots >= base, reusing a
// slot once its occupant's interval has ended. Returns the first free slot above
// the packed region (the staging base).
func (c *compiler) packWindow(base uint16) (uint16, error) {
	bStart, termPos, total := c.number()
	ranges := c.buildRanges(bStart, termPos, total)

	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].start != ranges[j].start {
			return ranges[i].start < ranges[j].start
		}
		return ranges[i].end < ranges[j].end
	})

	type active struct {
		end  int
		slot uint16
	}
	var live []active
	var free []uint16
	next := base
	top := base

	takeSlot := func() uint16 {
		if len(free) > 0 {
			min := 0
			for i := range free {
				if free[i] < free[min] {
					min = i
				}
			}
			s := free[min]
			free = append(free[:min], free[min+1:]...)
			return s
		}
		s := next
		next++
		return s
	}

	for _, r := range ranges {
		kept := live[:0]
		for _, a := range live {
			if a.end < r.start {
				free = append(free, a.slot)
			} else {
				kept = append(kept, a)
			}
		}
		live = kept

		slot := takeSlot()
		if int(slot) > maxReg {
			return 0, nil // caller reports the over-limit error via frameTop
		}
		if r.isConst {
			c.constSlot[r.id] = slot
		} else {
			c.reg[r.id] = slot
		}
		if slot+1 > top {
			top = slot + 1
		}
		live = append(live, active{end: r.end, slot: slot})
	}
	return top, nil
}

// number lays instructions out in reverse-postorder, one position per
// instruction plus a terminator slot per block. Returns each block's first
// position and its terminator position, and the total count.
func (c *compiler) number() (bStart, termPos map[*ir.Block]int, total int) {
	bStart = make(map[*ir.Block]int, len(c.cfg.RPO))
	termPos = make(map[*ir.Block]int, len(c.cfg.RPO))
	pos := 0
	for _, b := range c.cfg.RPO {
		bStart[b] = pos
		pos += len(b.Instrs)
		termPos[b] = pos
		pos++
	}
	return bStart, termPos, pos
}

// buildRanges derives the live interval of every non-parameter temporary (from
// block liveness plus in-block def/use positions). Constants are loaded once in
// the prologue and read directly from their slot, so a constant used inside a
// loop is re-read every iteration: rather than track that precisely, a constant
// is held live for the whole function ([0, total]). Its slot is never reused by a
// temporary, but the temporaries -- where the packing wins are -- still compact.
func (c *compiler) buildRanges(bStart, termPos map[*ir.Block]int, total int) []liveRange {
	n := len(c.f.Temps)
	tStart := make([]int, n)
	tEnd := make([]int, n)
	for i := range tStart {
		tStart[i] = math.MaxInt
		tEnd[i] = -1
	}
	obT := func(id uint32, p int) {
		if int(id) >= n {
			return
		}
		if p < tStart[id] {
			tStart[id] = p
		}
		if p > tEnd[id] {
			tEnd[id] = p
		}
	}
	obRef := func(r ir.Ref, p int) {
		if r.Kind == ir.RefTemp {
			obT(r.ID, p)
		}
	}

	live := c.cfg.Liveness()
	for _, b := range c.cfg.RPO {
		bs, be := bStart[b], termPos[b]
		for _, id := range live.LiveIn(b).Members() {
			obT(uint32(id), bs)
		}
		for _, id := range live.LiveOut(b).Members() {
			obT(uint32(id), be)
		}
		for _, p := range b.Phis {
			if p.To.IsTemp() {
				obT(p.To.ID, bs)
			}
		}
		for k := range b.Instrs {
			ip := bs + k
			in := &b.Instrs[k]
			if in.To.IsTemp() {
				obT(in.To.ID, ip)
			}
			start := 0
			if in.Op == ir.OCall {
				start = 1
			}
			for _, a := range in.Args[start:] {
				obRef(a, ip)
			}
		}
		obRef(b.Jmp.Arg, be)
		for _, a := range b.Jmp.Args {
			obRef(a, be)
		}
	}

	var ranges []liveRange
	for id := 0; id < n; id++ {
		if c.isParam[id] || tEnd[id] < 0 {
			continue // parameters are pinned; unobserved temps need no slot
		}
		s := tStart[id]
		if s == math.MaxInt {
			s = tEnd[id]
		}
		ranges = append(ranges, liveRange{start: s, end: tEnd[id], id: uint32(id)})
	}
	for id := range c.constPool {
		ranges = append(ranges, liveRange{start: 0, end: total, isConst: true, id: id})
	}
	return ranges
}
