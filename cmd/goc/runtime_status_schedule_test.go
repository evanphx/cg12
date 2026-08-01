package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runtimeCapabilitySensitivityPatterns are the things in a capability's source
// that make its outcome depend on how much of the machine it has. Each one is a
// reason to run the program alone.
var runtimeCapabilitySensitivityPatterns = map[string]*regexp.Regexp{
	"measures or waits on wall clock": regexp.MustCompile(
		`\btime\.(Now|Since|Sleep|After|AfterFunc|Tick|NewTimer|NewTicker)\b`),
	"bounds an operation by wall clock": regexp.MustCompile(
		`Set(Read|Write)?Deadline\b|DialTimeout|WithTimeout|WithDeadline`),
	"sets its own GOMAXPROCS": regexp.MustCompile(`GOMAXPROCS`),
	"asserts allocation or GC statistics": regexp.MustCompile(
		`ReadMemStats|MemStats|AllocsPerRun|NumGC\b|\bMallocs\b|\bFrees\b|HeapAlloc|TotalAlloc|HeapObjects`),
	"changes a process-wide runtime limit": regexp.MustCompile(`SetGCPercent|SetMemoryLimit|SetMaxThreads`),
	"asserts a goroutine count":            regexp.MustCompile(`runtime\.NumGoroutine`),
	"yields to the scheduler or the collector and then asserts what happened": regexp.MustCompile(
		`runtime\.Gosched\b`),
}

// runtimeCapabilityExclusiveCategories are categories whose whole point is to
// saturate the scheduler or the poller. What they measure is contention, so
// adding unrelated contention changes the thing under test.
var runtimeCapabilityExclusiveCategories = map[string]bool{
	"scheduler-stress":      true,
	"stdlib-netpoll-stress": true,
}

// TestRuntimeCapabilityExclusiveClassification keeps the exclusive set honest.
//
// The run phase runs most capabilities concurrently, which is only sound because
// the ones whose outcome depends on how much of the machine they have are marked
// exclusive and run alone. Nothing in a new capability's text makes its author
// think about that, so this test does the thinking mechanically: a source that
// measures or waits on wall clock, sets a deadline, asserts an allocation or GC
// statistic, sets its own GOMAXPROCS, or lives in a stress category must be
// marked exclusive.
//
// The rule is a floor rather than an equivalence. A capability can need
// isolation for a reason no pattern finds, so an extra marking is allowed and a
// missing one is not. Getting this wrong produces a flaky suite, which is worse
// than a slow one, and a fast unit test is a much better place to find out than
// the matrix.
func TestRuntimeCapabilityExclusiveClassification(t *testing.T) {
	for _, capability := range runtimeCapabilities() {
		source := filepath.Join("..", "..", "goc", "testdata", capability.source)
		contents, err := os.ReadFile(source)
		require.NoErrorf(t, err, "read %s", capability.source)

		reasons := runtimeCapabilitySensitivityReasons(capability, string(contents))
		if len(reasons) == 0 {
			continue
		}
		// assert rather than require: an author who has added several
		// capabilities wants the whole list, not the first one.
		assert.Truef(
			t,
			capability.exclusive,
			"%s/%s (%s) must be marked exclusive: it %v",
			capability.category, capability.name, capability.source, reasons,
		)
	}
}

func runtimeCapabilitySensitivityReasons(capability runtimeCapability, source string) []string {
	var reasons []string
	if runtimeCapabilityExclusiveCategories[capability.category] {
		reasons = append(reasons, "is in the "+capability.category+" category")
	}
	for reason, pattern := range runtimeCapabilitySensitivityPatterns {
		if pattern.MatchString(source) {
			reasons = append(reasons, reason)
		}
	}
	// The env field is the other way a capability's configuration stops being
	// the run phase's. Pinning GOMAXPROCS makes the program's share of the
	// machine its own business, and a GODEBUG diagnostic that walks stacks or
	// validates every buffered write barrier changes how long the program takes
	// by enough that its neighbours notice. Neither is visible in the source, so
	// the source patterns above cannot find them.
	for _, entry := range capability.env {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		switch name {
		case "GOMAXPROCS":
			reasons = append(reasons, "pins GOMAXPROCS through its env field")
		case "GODEBUG":
			reasons = append(reasons, "runs under a GODEBUG diagnostic set by its env field")
		}
	}
	return reasons
}

// TestRuntimeCapabilityCompileCostResolvesTheVendoredStandardLibrary is the
// guard on the compile-cost estimator. The estimator treats an unresolvable
// import as free, so a broken build context does not fail loudly: it returns the
// capability source's own size for everything, the queue's order collapses back
// to matrix order, and the matrix silently gets slower. This asserts that the
// vendored tree is actually being read.
func TestRuntimeCapabilityCompileCostResolvesTheVendoredStandardLibrary(t *testing.T) {
	model := newRuntimeCapabilityCostModel()

	runtimeClosure := model.closureSize("runtime")
	require.Greaterf(t, runtimeClosure, int64(1<<20),
		"the runtime's own import closure should be megabytes of source, got %d bytes", runtimeClosure)

	httpClosure := model.closureSize("net/http")
	require.Greater(t, httpClosure, runtimeClosure,
		"net/http's closure should be larger than the runtime's")
}

// TestRuntimeCapabilityCompileCostRanksTheExpensivePrograms checks the estimator
// against the shape the measurements found: the six net/http programs are the six
// most expensive things in the matrix, and a bare-runtime program is nowhere near
// them. The bound is twelve rather than six so that adding one more expensive
// program does not fail the test, while a model that has stopped separating
// programs at all -- which shows up as every capability tying and the order
// collapsing back to matrix order, where these sit at indices 218-223 -- still
// does.
func TestRuntimeCapabilityCompileCostRanksTheExpensivePrograms(t *testing.T) {
	ordered := runtimeCapabilitiesByDescendingCompileCost(runtimeCapabilities())
	require.Len(t, ordered, len(runtimeCapabilities()))

	position := make(map[string]int, len(ordered))
	for index, capability := range ordered {
		position[capability.category+"/"+capability.name] = index
	}

	for name, rank := range position {
		if !hasRuntimeCapabilityCategory(name, "stdlib-http/") {
			continue
		}
		require.Lessf(t, rank, 12, "%s should rank in the twelve most expensive, ranked %d", name, rank)
	}

	require.Greater(
		t,
		position["core-types/maps-slices-interfaces"],
		position["stdlib-http/tls-client-server"],
		"a bare-runtime program should rank cheaper than a net/http program",
	)
}

func hasRuntimeCapabilityCategory(name, prefix string) bool {
	return len(name) >= len(prefix) && name[:len(prefix)] == prefix
}
