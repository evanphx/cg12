package goc

import (
	"go/ast"
	"sync"
	"time"
)

// This file is measurement scaffolding for the ESCAPE_IR_PLAN.md design spike.
// It records what the demand-driven AST escape walk costs: how many times
// astParents rebuilds a parent map, over how many AST nodes, and how often the
// same cross-function summary question is asked again.
//
// It is off unless EscapeCostRecording is enabled, and it is not consulted by
// any compilation decision.

// EscapeCostKind names which caller rebuilt a parent map.
type EscapeCostKind int

const (
	// EscapeCostLowering is a parent map built once per function (or per
	// function literal, or per package-level initializer) as that body is
	// lowered. It is amortized over the whole body.
	EscapeCostLowering EscapeCostKind = iota
	// EscapeCostSummary is a parent map built by parameterDoesNotEscape,
	// parameterLeaksOnlyToResult or receiverDoesNotEscape to answer one
	// cross-function query. It is rebuilt on every query.
	EscapeCostSummary
	// EscapeCostReach is goc/reach.go's own walk, which is not part of the
	// escape analysis.
	EscapeCostReach
	escapeCostKinds
)

// EscapeCostStats is one compilation's worth of escape-walk cost.
type EscapeCostStats struct {
	// Calls counts astParents invocations per kind.
	Calls [escapeCostKinds]int64
	// Nodes counts AST nodes visited per kind, which is the size of the map
	// built and the real cost of the call.
	Nodes [escapeCostKinds]int64
	// Queries counts cross-function summary questions asked.
	Queries int64
	// DistinctQueries counts how many of those were about a
	// (function, index, summary) triple not asked before in this compilation.
	DistinctQueries int64
	// LargestSummaryBody is the node count of the biggest body a summary query
	// rebuilt a parent map over.
	LargestSummaryBody int64
	// WalkNanos is wall-clock time inside the escape walk, counted only at the
	// outermost entry so nested queries are not double-counted.
	WalkNanos int64
	// WalkEntries counts outermost escape queries -- the number of times the
	// front end asked the walk anything at all.
	WalkEntries int64
}

var escapeCost struct {
	sync.Mutex
	on    bool
	stats EscapeCostStats
	seen  map[escapeQueryKey]bool
}

type escapeQueryKey struct {
	name    string
	index   int
	summary bool
}

// StartEscapeCostRecording begins recording and clears any previous run's
// counts. It is intended for a single compilation at a time.
func StartEscapeCostRecording() {
	escapeCost.Lock()
	defer escapeCost.Unlock()
	escapeCost.on = true
	escapeCost.stats = EscapeCostStats{}
	escapeCost.seen = make(map[escapeQueryKey]bool)
}

// StopEscapeCostRecording ends recording and returns what was recorded.
func StopEscapeCostRecording() EscapeCostStats {
	escapeCost.Lock()
	defer escapeCost.Unlock()
	escapeCost.on = false
	stats := escapeCost.stats
	escapeCost.seen = nil
	return stats
}

func recordAstParents(kind EscapeCostKind, nodes int) {
	escapeCost.Lock()
	defer escapeCost.Unlock()
	if !escapeCost.on {
		return
	}
	escapeCost.stats.Calls[kind]++
	escapeCost.stats.Nodes[kind] += int64(nodes)
	if kind == EscapeCostSummary && int64(nodes) > escapeCost.stats.LargestSummaryBody {
		escapeCost.stats.LargestSummaryBody = int64(nodes)
	}
}

// escapeWalkDepth counts how deep the current escape query is nested. Only the
// outermost entry is timed. One compilation runs on one goroutine, which is
// what this counter assumes; recording is a measurement mode, not a compile
// mode.
var escapeWalkDepth int
var escapeWalkStart time.Time

// enterEscapeWalk marks the start of an escape query and returns the matching
// exit. Nested queries are no-ops.
func enterEscapeWalk() func() {
	if !escapeCost.on {
		return func() {}
	}
	escapeWalkDepth++
	if escapeWalkDepth != 1 {
		return func() { escapeWalkDepth-- }
	}
	escapeWalkStart = time.Now()
	return func() {
		escapeWalkDepth--
		escapeCost.Lock()
		escapeCost.stats.WalkNanos += int64(time.Since(escapeWalkStart))
		escapeCost.stats.WalkEntries++
		escapeCost.Unlock()
	}
}

func recordSummaryQuery(name string, index int, summary bool) {
	escapeCost.Lock()
	defer escapeCost.Unlock()
	if !escapeCost.on {
		return
	}
	escapeCost.stats.Queries++
	key := escapeQueryKey{name: name, index: index, summary: summary}
	if !escapeCost.seen[key] {
		escapeCost.seen[key] = true
		escapeCost.stats.DistinctQueries++
	}
}

// astParentsFor is astParents with the cost of the call attributed to a caller
// category. The map holds one entry per node except the root, so its length is
// the walked node count minus one; no second traversal is needed.
func astParentsFor(kind EscapeCostKind, root ast.Node) map[ast.Node]ast.Node {
	parents := astParents(root)
	recordAstParents(kind, len(parents)+1)
	return parents
}

var _ = ast.Inspect

// PlacementCounts records, per front-end decision site, how many allocations
// that site put in the frame and how many it put in the heap. It exists to size
// the migration in ESCAPE_IR_PLAN.md: an allocation whose placement the AST
// walk commits cannot be revisited by an IR pass, so these are the decisions
// that would have to move.
type PlacementCounts struct {
	Frame int64
	Heap  int64
}

var placementCounts struct {
	sync.Mutex
	on     bool
	counts map[string]*PlacementCounts
}

// StartPlacementRecording begins recording allocation placements.
func StartPlacementRecording() {
	placementCounts.Lock()
	defer placementCounts.Unlock()
	placementCounts.on = true
	placementCounts.counts = make(map[string]*PlacementCounts)
}

// StopPlacementRecording ends recording and returns what was recorded.
func StopPlacementRecording() map[string]PlacementCounts {
	placementCounts.Lock()
	defer placementCounts.Unlock()
	placementCounts.on = false
	out := make(map[string]PlacementCounts, len(placementCounts.counts))
	for site, counts := range placementCounts.counts {
		out[site] = *counts
	}
	placementCounts.counts = nil
	return out
}

func recordPlacement(site string, heap bool) {
	placementCounts.Lock()
	defer placementCounts.Unlock()
	if !placementCounts.on {
		return
	}
	counts, ok := placementCounts.counts[site]
	if !ok {
		counts = &PlacementCounts{}
		placementCounts.counts[site] = counts
	}
	if heap {
		counts.Heap++
	} else {
		counts.Frame++
	}
}
