package arm64

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// dumpPressureReport writes a register-allocation analysis for the function named by
// CG12_DUMP_REPORT to CG12_DUMP_REPORT_FILE (default report.txt). Unlike the JSON
// range dump (which uses the allocator's conservative whole-range intervals), this
// reconstructs PRECISE per-instruction liveness backward from each block's live-out,
// so the pressure numbers reflect what the colourer actually saw. It answers the one
// question that decides the strategy: is the spilling forced by genuine
// over-subscription (more values live across a call than there are callee-saved
// registers), or is it a colouring inefficiency (pressure fits, yet we spill)?
func dumpPressureReport(f *ir.Func, alloc *allocation, num *numbering, cfg *analysis.CFG, live *analysis.Liveness, freq *analysis.Freq) {
	if os.Getenv("CG12_DUMP_REPORT") != f.Name {
		return
	}
	isInt := func(t int) bool {
		tt := f.Temps[t]
		return tt != nil && !tt.Cls.IsFloat()
	}

	// Per-temp static use/def counts and frequency-weighted use count.
	uses := make([]int, len(f.Temps))
	defs := make([]int, len(f.Temps))
	wuse := make([]float64, len(f.Temps))
	blockOf := map[*ir.Instr]*ir.Block{}
	for _, b := range cfg.RPO {
		fr := freq.Of(b)
		for k := range b.Instrs {
			in := &b.Instrs[k]
			blockOf[in] = b
			for _, u := range instrUses(in) {
				uses[u]++
				wuse[u] += fr
			}
			for _, d := range instrDefs(in) {
				defs[d]++
			}
		}
		if b.Jmp.Arg.Kind == ir.RefTemp {
			uses[b.Jmp.Arg.ID]++
			wuse[b.Jmp.Arg.ID] += fr
		}
		for _, a := range b.Jmp.Args {
			if a.Kind == ir.RefTemp {
				uses[a.ID]++
				wuse[a.ID] += fr
			}
		}
	}

	// Precise pressure by walking each block backward from its live-out set.
	maxP, maxPBlock, maxPFreq := 0, "", 0.0
	var maxPLive []int
	// pressure histogram: positions with GP pressure >= i
	hist := map[int]int{}
	// per-call cross pressure
	type callInfo struct {
		block string
		freq  float64
		cross []int // int temps live across this call
	}
	var worstCall callInfo
	var calls []callInfo
	for _, b := range cfg.RPO {
		fr := freq.Of(b)
		liveAfter := live.LiveOut(b).Copy()
		for k := len(b.Instrs) - 1; k >= 0; k-- {
			in := &b.Instrs[k]
			// GP pressure at this instruction = int temps live-out of it.
			gp := 0
			for _, t := range liveAfter.Members() {
				if isInt(t) {
					gp++
				}
			}
			for i := 1; i <= gp; i++ {
				hist[i]++
			}
			if gp > maxP {
				maxP, maxPBlock, maxPFreq = gp, b.Name, fr
				maxPLive = nil
				for _, t := range liveAfter.Members() {
					if isInt(t) {
						maxPLive = append(maxPLive, t)
					}
				}
			}
			// live-before = liveAfter - defs + uses
			liveBefore := liveAfter.Copy()
			for _, d := range instrDefs(in) {
				liveBefore.Remove(d)
			}
			for _, u := range instrUses(in) {
				liveBefore.Add(u)
			}
			if in.Op == ir.OCall {
				// live across = live both before and after (survives the call), int only.
				var cross []int
				for _, t := range liveAfter.Members() {
					if isInt(t) && liveBefore.Has(t) {
						cross = append(cross, t)
					}
				}
				ci := callInfo{block: b.Name, freq: fr, cross: cross}
				calls = append(calls, ci)
				if len(cross) > len(worstCall.cross) {
					worstCall = ci
				}
			}
			liveAfter = liveBefore
		}
	}

	alloced := func(t int) string {
		tt := f.Temps[t]
		if tt.Reg != ir.NoReg {
			return Reg(tt.Reg).Name(8)
		}
		if r, ok := alloc.remat[t]; ok {
			return []string{"remat?", "remat-alloca", "remat-const", "remat-sym"}[r.kind]
		}
		if tt.Slot == -1 {
			return "remat"
		}
		return fmt.Sprintf("spill[%d]", tt.Slot)
	}
	ivByTemp := map[int]*interval{}
	for _, iv := range alloc.intervals {
		ivByTemp[iv.temp] = iv
	}
	span := func(t int) (int, int) {
		if iv := ivByTemp[t]; iv != nil {
			return iv.start, iv.end
		}
		return -1, -1
	}
	crosses := func(t int) bool {
		if iv := ivByTemp[t]; iv != nil {
			return iv.crosses
		}
		return false
	}

	var sb strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&sb, format, a...) }

	// count allocatable-register facts
	spilled := 0
	regd := 0
	for t := range f.Temps {
		if f.Temps[t] == nil || !isInt(t) {
			continue
		}
		if iv := ivByTemp[t]; iv == nil {
			continue // never live
		}
		if f.Temps[t].Reg != ir.NoReg {
			regd++
		} else if _, ok := alloc.remat[t]; !ok && f.Temps[t].Slot != -1 {
			spilled++
		}
	}

	p("=== REGISTER-ALLOCATION REPORT: %s ===\n", f.Name)
	p("%d temps, %d blocks, %d positions\n", len(f.Temps), len(cfg.RPO), len(num.posInstr))
	p("integer temps live: %d in registers, %d spilled\n\n", regd, spilled)

	p("--- PRESSURE (precise per-instruction liveness) ---\n")
	p("peak GP pressure = %d  (block %s, freq %.1f)\n", maxP, maxPBlock, maxPFreq)
	p("positions with GP pressure >= N:\n")
	for _, n := range []int{10, 12, 14, 16, 18, 20, 24, 28, 32} {
		if hist[n] > 0 {
			p("   >=%-2d : %d positions\n", n, hist[n])
		}
	}
	p("\npeak-pressure live set (%d int temps):\n", len(maxPLive))
	sort.Slice(maxPLive, func(i, j int) bool {
		a, b := maxPLive[i], maxPLive[j]
		sa, _ := span(a)
		sb2, _ := span(b)
		return sa < sb2
	})
	for _, t := range maxPLive {
		s, e := span(t)
		p("   %-24s %-14s span[%d..%d] uses=%d cross=%v\n", f.Temps[t].Name, alloced(t), s, e, uses[t], crosses(t))
	}

	p("\n--- LIVE-ACROSS-CALL PRESSURE (decisive metric) ---\n")
	p("%d calls in function. callee-saved GP registers available: 10 (x19-x28)\n", len(calls))
	p("worst call: block %s (freq %.1f) has %d int temps live across it:\n", worstCall.block, worstCall.freq, len(worstCall.cross))
	sort.Slice(worstCall.cross, func(i, j int) bool {
		return wuse[worstCall.cross[i]] > wuse[worstCall.cross[j]]
	})
	for _, t := range worstCall.cross {
		s, e := span(t)
		p("   %-24s %-14s span[%d..%d] uses=%d wuse=%.0f\n", f.Temps[t].Name, alloced(t), s, e, uses[t], wuse[t])
	}
	// histogram of live-across-call counts
	acrossHist := map[int]int{}
	for _, c := range calls {
		acrossHist[len(c.cross)]++
	}
	p("\ndistribution of #int-temps-live-across-call over all %d calls:\n", len(calls))
	var keys []int
	for k := range acrossHist {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		p("   %2d across : %d calls\n", k, acrossHist[k])
	}

	// The spilled set: the values the allocator gave up on.
	p("\n--- SPILLED INTEGER TEMPS (sorted by frequency-weighted use) ---\n")
	var sp []int
	for t := range f.Temps {
		if f.Temps[t] == nil || !isInt(t) {
			continue
		}
		if iv := ivByTemp[t]; iv == nil {
			continue
		}
		if f.Temps[t].Reg == ir.NoReg {
			if _, ok := alloc.remat[t]; !ok && f.Temps[t].Slot != -1 {
				sp = append(sp, t)
			}
		}
	}
	sort.Slice(sp, func(i, j int) bool { return wuse[sp[i]] > wuse[sp[j]] })
	p("name                     slot        span            defs uses wuse   cross\n")
	for _, t := range sp {
		s, e := span(t)
		p("%-24s %-11s [%5d..%5d]  %3d  %3d  %5.0f  %v\n",
			f.Temps[t].Name, alloced(t), s, e, defs[t], uses[t], wuse[t], crosses(t))
	}

	path := os.Getenv("CG12_DUMP_REPORT_FILE")
	if path == "" {
		path = "report.txt"
	}
	_ = os.WriteFile(path, []byte(sb.String()), 0o644)
}
