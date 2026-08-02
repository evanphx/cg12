package goc_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// TestAllocationPlacementCensus counts, over the whole corpus, how many
// allocations the front end's AST escape walk places itself and how many it
// leaves as an ir.OHeapAlloc candidate for opt.LowerHeapAllocations to place.
//
// It exists for ESCAPE_IR_PLAN.md. Moving the analysis onto the IR means every
// placement the AST walk commits has to be turned into a neutral op first, so
// the size of that number is the size of the migration.
//
// It is a measurement, not an assertion: it prints and passes.
func TestAllocationPlacementCensus(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the whole corpus")
	}
	programs, err := filepath.Glob("testdata/*.go")
	require.NoError(t, err)
	require.NotEmpty(t, programs)

	limit := len(programs)
	if value := os.Getenv("PLACEMENT_CENSUS_LIMIT"); value != "" {
		fmt.Sscanf(value, "%d", &limit)
		if limit > len(programs) {
			limit = len(programs)
		}
	}
	programs = programs[:limit]

	opt.ResetHeapAllocLoweringStats()
	goc.StartPlacementRecording()
	compiled := 0
	irFrame, irHeap := 0, 0
	for _, program := range programs {
		source, err := os.ReadFile(program)
		require.NoError(t, err)
		module, err := goc.CompileExecutable(program, source)
		require.NoError(t, err, program)
		compiled++
		frame, heap := countModuleAllocations(module)
		irFrame += frame
		irHeap += heap
	}
	counts := goc.StopPlacementRecording()
	promoted, lowered := opt.HeapAllocLoweringStats()

	sites := make([]string, 0, len(counts))
	for site := range counts {
		sites = append(sites, site)
	}
	sort.Strings(sites)

	report := map[string]any{
		"programs":               compiled,
		"emittedFrameAllocs":     irFrame,
		"emittedAllocatorCal":    irHeap,
		"neutralPromotedToFrame": promoted,
		"neutralLoweredToHeap":   lowered,
		"sites":                  map[string]any{},
	}
	for _, site := range sites {
		report["sites"].(map[string]any)[site] = map[string]int64{
			"frame": counts[site].Frame,
			"heap":  counts[site].Heap,
		}
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	require.NoError(t, err)
	fmt.Printf("PLACEMENT_CENSUS %s\n", encoded)
}

// countModuleAllocations counts frame allocations and allocator calls in the
// finished IR: the same census section 5 of the escape-frame-publication report
// used.
func countModuleAllocations(module *ir.Module) (int, int) {
	frame, heap := 0, 0
	for _, function := range module.Funcs {
		if function.Start == nil {
			continue
		}
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				if instruction.Op.IsAlloc() || instruction.Op == ir.OAllocN {
					frame++
					continue
				}
				if instruction.Op == ir.OHeapAlloc {
					// Should not survive LowerHeapAllocations; counted so a
					// leak would be visible rather than silent.
					heap++
					continue
				}
				if instruction.Op != ir.OCall || len(instruction.Args) == 0 {
					continue
				}
				callee := instruction.Arg(0)
				if callee.Kind != ir.RefConst {
					continue
				}
				constant := function.Consts[callee.ID]
				if constant.Kind != ir.ConstSym {
					continue
				}
				switch constant.Sym {
				case "runtime.newobject", "runtime.newarray", "runtime.makeslice",
					"runtime.mallocgc", "runtime.makemap", "runtime.makemap_small",
					"runtime.makechan":
					heap++
				}
			}
		}
	}
	return frame, heap
}
