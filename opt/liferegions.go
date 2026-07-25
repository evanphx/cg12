package opt

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/ir"
)

// mem2reg runs twice in the pipeline; the first run promotes the full set of
// marked locals, the second (post-inline) sees only a residue whose markers the
// first run already dropped. Capture only the first run so its dump is not
// clobbered by the residue.
var lifeRegionsOnce sync.Once

// dumpLifeRegions is an env-gated (CG12_DUMP_LIFEREGIONS=<func>) read-only
// diagnostic. For each promotable, alloca-lifetime-marked variable it reports how
// large the variable's lifetime region is (derived from its OLifeStart/OLifeEnd
// markers via the same forward, marker-gated dataflow the frame allocator's
// allocaGroups uses) relative to the whole function, and whether that region
// contains a call or safepoint.
//
// Correlated by variable name with the backend spill report, it answers the one
// question that decides whether clamping a promoted value's register-allocation
// liveness to its source lifetime would help: a spilled per-handler local with a
// narrow, safepoint-free region is a register candidate the mesh's conservative
// liveness spuriously spills; a dispatch-scoped value whose region is the whole
// function would not move. It must run while the markers still exist -- before
// renameBlock drops them for a promoted alloca -- so it is called from Mem2Reg.
func dumpLifeRegions(f *ir.Func, cfg *analysis.CFG, vars []promotable, varOf map[uint32]int) {
	if os.Getenv("CG12_DUMP_LIFEREGIONS") != f.Name {
		return
	}
	n := len(vars)
	if n == 0 {
		return
	}

	// Which variables carry each marker kind. A variable is bounded only with both.
	hasStart := make([]bool, n)
	hasEnd := make([]bool, n)
	for _, b := range f.Blocks {
		for k := range b.Instrs {
			in := &b.Instrs[k]
			if !in.Op.IsLifetime() || in.Args[0].Kind != ir.RefTemp {
				continue
			}
			vi, ok := varOf[in.Args[0].ID]
			if !ok {
				continue
			}
			if in.Op == ir.OLifeStart {
				hasStart[vi] = true
			} else {
				hasEnd[vi] = true
			}
		}
	}
	anyBounded := false
	for vi := range vars {
		if hasStart[vi] && hasEnd[vi] {
			anyBounded = true
			break
		}
	}
	// Nothing to report (e.g. CG12_LIFETIMES off, or the second mem2reg run whose
	// vars were already promoted and had their markers dropped). Return without
	// writing so an earlier run's file is preserved.
	if !anyBounded {
		return
	}

	// Forward, marker-gated region dataflow: a start turns a variable on, an end off;
	// a variable is "in region" for any block where it is on anywhere. This mirrors
	// arm64/allocacolor.go allocaGroups, keyed here on the variable index.
	// Initialize over every block (not just the RPO) so a predecessor that RPO does
	// not reach still has a (zero) liveOut entry rather than a nil slice.
	liveOut := map[*ir.Block][]bool{}
	blockLive := map[*ir.Block][]bool{}
	for _, b := range f.Blocks {
		liveOut[b] = make([]bool, n)
		blockLive[b] = make([]bool, n)
	}
	for changed := true; changed; {
		changed = false
		for _, b := range cfg.RPO {
			cur := make([]bool, n)
			for _, p := range b.Preds {
				lo := liveOut[p]
				for i := 0; i < n; i++ {
					cur[i] = cur[i] || lo[i]
				}
			}
			bl := make([]bool, n)
			copy(bl, cur)
			for k := range b.Instrs {
				in := &b.Instrs[k]
				if in.Op.IsLifetime() && in.Args[0].Kind == ir.RefTemp {
					if vi, ok := varOf[in.Args[0].ID]; ok {
						cur[vi] = in.Op == ir.OLifeStart
					}
				}
				for i := 0; i < n; i++ {
					bl[i] = bl[i] || cur[i]
				}
			}
			blockLive[b] = bl
			old := liveOut[b]
			for i := 0; i < n; i++ {
				if cur[i] != old[i] {
					changed = true
					break
				}
			}
			liveOut[b] = cur
		}
	}

	// A block is a safepoint if it holds a call or an explicit OSafepoint.
	safeBlk := map[*ir.Block]bool{}
	for _, b := range cfg.RPO {
		for k := range b.Instrs {
			if op := b.Instrs[k].Op; op == ir.OCall || op == ir.OSafepoint {
				safeBlk[b] = true
				break
			}
		}
	}

	total := len(cfg.RPO)
	type row struct {
		name         string
		start, end   bool
		region       int
		safeInRegion bool
	}
	rows := make([]row, 0, n)
	for vi := range vars {
		region, safe := 0, false
		for _, b := range cfg.RPO {
			if blockLive[b][vi] {
				region++
				if safeBlk[b] {
					safe = true
				}
			}
		}
		rows = append(rows, row{varBase(vars[vi].name), hasStart[vi], hasEnd[vi], region, safe})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].region < rows[j].region })

	var sb strings.Builder
	fmt.Fprintf(&sb, "=== LIFETIME REGIONS: %s (%d promotable vars, %d blocks) ===\n", f.Name, n, total)
	fmt.Fprintf(&sb, "%-28s start end   region/total  safepoint_in_region\n", "var")
	bounded, boundedSafeFree := 0, 0
	for _, r := range rows {
		isBounded := r.start && r.end && r.region < total
		if isBounded {
			bounded++
			if !r.safeInRegion {
				boundedSafeFree++
			}
		}
		fmt.Fprintf(&sb, "%-28s %-5v %-5v %5d/%-5d   %v\n", r.name, r.start, r.end, r.region, total, r.safeInRegion)
	}
	fmt.Fprintf(&sb, "\nsummary: %d/%d vars bounded (start+end, region<total); "+
		"%d of those are safepoint-free (register candidates once clamped)\n", bounded, n, boundedSafeFree)

	lifeRegionsOnce.Do(func() {
		path := os.Getenv("CG12_DUMP_LIFEREGIONS_FILE")
		if path == "" {
			path = "liferegions.txt"
		}
		_ = os.WriteFile(path, []byte(sb.String()), 0o644)
	})
}
