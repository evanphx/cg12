package goc_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

var escapeCostProgram = flag.String("escape-cost-program", "testdata/runtime_slice_pointer_append_gc.go",
	"program to compile when measuring the escape walk's cost")

// TestEscapeWalkCost measures what the demand-driven AST escape walk costs on
// one representative whole-program compile: how many parent maps it rebuilds,
// over how many AST nodes, and how many of its cross-function summary queries
// repeat a question already answered.
//
// It exists for ESCAPE_IR_PLAN.md, which proposes replacing the walk. A claim
// about the cost of what is there now has to be measured, not estimated.
func TestEscapeWalkCost(t *testing.T) {
	source, err := os.ReadFile(*escapeCostProgram)
	require.NoError(t, err)

	goc.StartEscapeCostRecording()
	start := time.Now()
	module, err := goc.CompileExecutable(*escapeCostProgram, source)
	elapsed := time.Since(start)
	stats := goc.StopEscapeCostRecording()
	require.NoError(t, err)

	// The closest available proxy for what an IR-level replacement would cost:
	// one whole-module, per-function pointer dataflow analysis over the same
	// program, run to a fixed point.
	checkStart := time.Now()
	findings := opt.FrameEscapes(module)
	checkElapsed := time.Since(checkStart)

	report := map[string]any{
		"program":              *escapeCostProgram,
		"frameEscapesSeconds":  checkElapsed.Seconds(),
		"frameEscapesFindings": len(findings),
		"compileSeconds":       elapsed.Seconds(),
		"functionsEmitted":     countFunctions(module),
		"summaryCalls":         stats.Calls[goc.EscapeCostSummary],
		"summaryNodes":         stats.Nodes[goc.EscapeCostSummary],
		"summaryLargestBody":   stats.LargestSummaryBody,
		"loweringCalls":        stats.Calls[goc.EscapeCostLowering],
		"loweringNodes":        stats.Nodes[goc.EscapeCostLowering],
		"reachCalls":           stats.Calls[goc.EscapeCostReach],
		"reachNodes":           stats.Nodes[goc.EscapeCostReach],
		"summaryQueries":       stats.Queries,
		"walkSeconds":          float64(stats.WalkNanos) / 1e9,
		"walkEntries":          stats.WalkEntries,
		"walkShareOfCompile":   float64(stats.WalkNanos) / float64(elapsed.Nanoseconds()),
		"distinctQueries":      stats.DistinctQueries,
		"queriesPerDistinct":   ratio(stats.Queries, stats.DistinctQueries),
		"nodesPerSummaryCall":  ratio(stats.Nodes[goc.EscapeCostSummary], stats.Calls[goc.EscapeCostSummary]),
		"summaryNodesOverBody": ratio(stats.Nodes[goc.EscapeCostSummary], stats.Nodes[goc.EscapeCostLowering]),
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	require.NoError(t, err)
	fmt.Printf("ESCAPE_COST %s\n", encoded)
}

func ratio(numerator, denominator int64) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func countFunctions(module *ir.Module) int {
	count := 0
	for _, function := range module.Funcs {
		if function.Start != nil {
			count++
		}
	}
	return count
}
