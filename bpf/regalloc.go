package bpf

import (
	"sort"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// Register model. r10 is the read-only frame pointer; r0 and r1 are reserved as
// scratch (spill reload, parallel-move rescue, and the ABI's result/first-arg
// registers). Temporaries are allocated from r2-r9. r6-r9 are preserved across
// helper calls by the kernel, so a value live across a call must land there (or
// spill); r2-r5 are caller-saved and used first for short-lived values.
const (
	scratchA = R0
	scratchB = R1
)

var allocOrder = []Reg{R2, R3, R4, R5, R6, R7, R8, R9}

func calleeSaved(r Reg) bool { return r >= R6 && r <= R9 }

// interval is a temporary's conservative live range over a linear numbering.
type interval struct {
	temp    int
	start   int
	end     int
	crosses bool // live across a call -> needs a callee-saved register
}

// regAlloc assigns every temporary either a register (Temp.Reg) or a spill slot
// (Temp.Slot), by linear scan over liveness intervals.
func (c *comp) regAlloc() {
	cfg := analysis.BuildCFG(c.f)
	live := cfg.Liveness()

	pos := map[*ir.Block][2]int{}
	var callAt []int
	n := 0
	for _, b := range cfg.RPO {
		first := n
		for k := range b.Instrs {
			if b.Instrs[k].Op == ir.OCall {
				callAt = append(callAt, n)
			}
			n++
		}
		n++ // terminator slot
		pos[b] = [2]int{first, n - 1}
	}

	const inf = 1 << 30
	ivs := make([]*interval, len(c.f.Temps))
	for i := range ivs {
		ivs[i] = &interval{temp: i, start: inf, end: -1}
	}
	extend := func(id, p int) {
		if p < ivs[id].start {
			ivs[id].start = p
		}
		if p > ivs[id].end {
			ivs[id].end = p
		}
	}
	touch := func(r ir.Ref, p int) {
		if r.Kind == ir.RefTemp {
			extend(int(r.ID), p)
		}
	}
	for _, b := range cfg.RPO {
		bp := pos[b]
		for _, id := range live.LiveIn(b).Members() {
			extend(id, bp[0])
		}
		for _, id := range live.LiveOut(b).Members() {
			extend(id, bp[1])
		}
		p := bp[0]
		for _, phi := range b.Phis {
			touch(phi.To, bp[0])
		}
		for k := range b.Instrs {
			in := &b.Instrs[k]
			for _, a := range in.Args {
				touch(a, p)
			}
			touch(in.To, p)
			p++
		}
		touch(b.Jmp.Arg, bp[1])
	}

	var intervals []*interval
	for _, iv := range ivs {
		if iv.end < 0 {
			continue
		}
		for _, cp := range callAt {
			if iv.start <= cp && cp < iv.end {
				iv.crosses = true
				break
			}
		}
		intervals = append(intervals, iv)
	}

	c.linearScan(intervals)
}

func (c *comp) linearScan(intervals []*interval) {
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].start != intervals[j].start {
			return intervals[i].start < intervals[j].start
		}
		return intervals[i].end < intervals[j].end
	})

	active := []*interval{}
	inUse := map[Reg]bool{}

	expire := func(start int) {
		kept := active[:0]
		for _, a := range active {
			if a.end < start {
				delete(inUse, Reg(c.f.Temps[a.temp].Reg))
			} else {
				kept = append(kept, a)
			}
		}
		active = kept
	}
	pick := func(crosses bool) (Reg, bool) {
		for _, r := range allocOrder {
			if inUse[r] {
				continue
			}
			if crosses && !calleeSaved(r) {
				continue
			}
			return r, true
		}
		return 0, false
	}

	for _, iv := range intervals {
		expire(iv.start)
		if r, ok := pick(iv.crosses); ok {
			c.f.Temps[iv.temp].Reg = int(r)
			inUse[r] = true
			active = append(active, iv)
			sort.Slice(active, func(i, j int) bool { return active[i].end < active[j].end })
		} else {
			c.spill(iv.temp)
		}
	}
}

// spill gives a temporary a stack slot (a negative byte offset from r10).
func (c *comp) spill(temp int) {
	c.f.Temps[temp].Reg = ir.NoReg
	if _, ok := c.spillOff[temp]; !ok {
		c.stackTop -= 8
		c.spillOff[temp] = c.stackTop
	}
}
