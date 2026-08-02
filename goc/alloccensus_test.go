package goc_test

import (
	"flag"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// updateAllocCensusBaseline rewrites the accepted census from this run instead
// of comparing against it. It is deliberately a separate, explicit act: the
// whole value of the census is that a placement change has to be looked at by a
// person, and a baseline that any failing run could refresh on its own would be
// worth nothing.
var updateAllocCensusBaseline = flag.Bool("update-alloc-census-baseline", false,
	"rewrite testdata/alloc_census_baseline.txt from this run")

const allocCensusBaselinePath = "testdata/alloc_census_baseline.txt"

// TestAllocationCensus records where every allocation in the corpus lands --
// frame or heap -- and fails when any of them moves in either direction.
//
// # Why this is a test and not a benchmark
//
// The escape decision has no answer that is right in isolation, which is why it
// keeps regressing quietly. Both directions are dangerous and neither shows up
// as a failing assertion anywhere else:
//
//   - An object that moves to the heap costs an allocation, a zeroing, and the
//     collector's attention, on a path that used to have none. Nothing fails; the
//     program just gets slower, and it stays slower because no one was watching.
//
//   - An object that moves to the frame is a possible correctness bug. It is how
//     ccwork/escape-analysis's 2724ac7 left a dead frame address inside a live
//     heap object, which passed that branch's entire test run and surfaced
//     minutes into an unrelated goroutine's GC as a bad pointer.
//
// The census that found that defect was written by hand, run once, and thrown
// away. It also caught an early draft of the escape-frame-publication fix
// silently reopening 9f76498's hole -- six functions that stopped allocating on
// the heap -- which no pass/fail test in the tree noticed. This file is that
// census made permanent.
//
// # What a reviewer should ask before accepting a diff
//
// Rewrite the baseline with
//
//	go test ./goc -run TestAllocationCensus -update-alloc-census-baseline
//
// and then read the diff. Every line answers "where did this object go", so:
//
//  1. For each site that moved HEAP -> FRAME, ask what proves the object cannot
//     outlive the frame. This is the correctness-critical direction. An object is
//     safe in a frame only if no pointer to it is stored into a global, into
//     another object, into the caller's result area, or into anything reached
//     through a parameter -- and only if it is not captured by a closure or a
//     goroutine that outlives the call. If the answer is "the analysis says so",
//     that is not an answer: the analysis saying so wrongly is the failure mode.
//     TestFrameEscapeAudit checks the emitted stores for exactly this and should
//     be read alongside this diff, but it only sees publications it can prove, so
//     a clean run there is necessary and not sufficient.
//
//  2. For each site that moved FRAME -> HEAP, ask whether the allocation is
//     really needed or whether the analysis simply lost track. Losing a promotion
//     is usually a widened may-escape rule catching a case it did not mean to.
//     Check whether the site is on a hot path -- the runtime's own allocator,
//     scheduler and GC paths are compiled here too, and an allocation added
//     inside them is a much bigger deal than one in a leaf helper.
//
//  3. For each site that DISAPPEARED, ask whether the allocation is gone or the
//     code is gone. A site vanishing because the function stopped being reachable
//     is fine; a heap site vanishing because the object is now an ordinary
//     front-end frame slot is the 9f76498 failure and is question 1 in disguise.
//     The census cannot tell these apart on its own -- it does not record
//     ordinary front-end frame slots (see opt.AllocationCensus) -- so this is the
//     case that needs the source looked at.
//
//  4. For each site that APPEARED, ask whether it is new code or a new
//     allocation in old code. Adding corpus programs adds sites; that is expected
//     and the diff should be confined to those programs' own files.
//
//  5. Ask whether the size of the diff matches the size of the change. A
//     one-line escape-analysis fix that moves four hundred sites has done
//     something other than what it says.
//
// Accepting a diff means someone answered these. That is the point of making the
// update an explicit flag rather than a self-healing baseline.
func TestAllocationCensus(t *testing.T) {
	audit := auditCorpus(t)
	found := audit.allocations

	if *updateAllocCensusBaseline {
		writeBaseline(t, allocCensusBaselinePath, allocCensusBaselineHeader, found)
		t.Skip("baseline rewritten; rerun without -update-alloc-census-baseline to check it")
	}

	accepted := readBaseline(t, allocCensusBaselinePath)
	movedToFrame, movedToHeap, appeared, vanished := compareAllocationCensus(found, accepted)

	assert.Empty(t, movedToFrame,
		"an allocation moved from the heap into a frame. This is the correctness-critical\n"+
			"direction: the object now lives in storage that dies when the function returns,\n"+
			"and if anything retains a pointer to it the collector will walk a dead frame.\n"+
			"For each site below, say what proves the object cannot outlive the call, then\n"+
			"rerun with -update-alloc-census-baseline. Read TestFrameEscapeAudit's result\n"+
			"alongside this: it checks the stores the compiler actually emitted.\n  %s",
		strings.Join(movedToFrame, "\n  "))
	assert.Empty(t, movedToHeap,
		"an allocation moved from a frame onto the heap. This is a possible performance\n"+
			"regression: the site now costs an allocation and the collector's attention where\n"+
			"it cost nothing. It usually means an escape rule was widened past what it meant\n"+
			"to catch. For each site below, say why the object has to be on the heap, then\n"+
			"rerun with -update-alloc-census-baseline.\n  %s",
		strings.Join(movedToHeap, "\n  "))
	assert.Empty(t, vanished,
		"%s lists an allocation site the compiler no longer has. That is fine if the code\n"+
			"is gone, and is a silent heap-to-frame move if the object is now an ordinary\n"+
			"front-end frame slot -- which this census does not record and so cannot tell you\n"+
			"apart. Read the source at each site below before rerunning with\n"+
			"-update-alloc-census-baseline.\n  %s",
		allocCensusBaselinePath, strings.Join(vanished, "\n  "))
	assert.Empty(t, appeared,
		"an allocation site exists that %s does not list. Expected when corpus programs or\n"+
			"stdlib code are added, in which case the sites below should all be in the files\n"+
			"that changed. A new heap site in untouched code is a frame-to-heap move the\n"+
			"census can only see from one side. Rerun with -update-alloc-census-baseline.\n  %s",
		allocCensusBaselinePath, strings.Join(appeared, "\n  "))
}

// compareAllocationCensus sorts the difference between this run's census and the
// accepted one into the four things a reviewer has to answer separately: an
// object that moved onto the frame, one that moved onto the heap, a site that is
// new, and a site that is gone. Each returned line names the site and says which
// way it went, because a count of changed lines tells a reviewer nothing about
// whether to accept them.
//
// found maps a census line to a program that produced it; accepted is the set of
// lines in the baseline.
func compareAllocationCensus(found map[string]string, accepted map[string]bool) (movedToFrame, movedToHeap, appeared, vanished []string) {
	foundSites := allocationSites(sortedKeys(found))
	acceptedSites := allocationSites(sortedSet(accepted))

	for _, site := range sortedSiteNames(foundSites) {
		now := foundSites[site]
		seenIn := found[site+"\t"+now]
		if seenIn == "" {
			seenIn = "several programs"
		}
		before, known := acceptedSites[site]
		if !known {
			appeared = append(appeared, fmt.Sprintf("%s\n      now on the %s, seen compiling %s", site, now, seenIn))
			continue
		}
		if before == now {
			continue
		}
		// A site can hold both placements at once -- one source position inlined
		// into one function twice, decided differently each time -- so the
		// direction is which placement the site gained or lost, not which of two
		// values it changed to. Gaining "frame" and losing "heap" are the same
		// news: some copy of this allocation that used to be on the heap is now in
		// a frame. Getting this wrong files a heap-to-frame move, the
		// correctness-critical one, under performance.
		line := fmt.Sprintf("%s\n      %s -> %s, seen compiling %s", site, before, now, seenIn)
		beforeFrame, beforeHeap := placedIn(before, "frame"), placedIn(before, "heap")
		nowFrame, nowHeap := placedIn(now, "frame"), placedIn(now, "heap")
		if (nowFrame && !beforeFrame) || (beforeHeap && !nowHeap) {
			movedToFrame = append(movedToFrame, line)
		}
		if (nowHeap && !beforeHeap) || (beforeFrame && !nowFrame) {
			movedToHeap = append(movedToHeap, line)
		}
	}
	for _, site := range sortedSiteNames(acceptedSites) {
		if _, ok := foundSites[site]; !ok {
			vanished = append(vanished, fmt.Sprintf("%s\n      was on the %s, now absent", site, acceptedSites[site]))
		}
	}
	return movedToFrame, movedToHeap, appeared, vanished
}

// allocationSites splits census lines into site -> placement. A line is the site
// (position, function, allocator, type) and its placement, tab separated, so the
// site survives a move and the move is the change in the last field.
//
// A site that appears with both placements -- the same source position inlined
// into the same function twice, decided differently each time -- is reported as
// "frame+heap" rather than one of them, so it neither hides the split nor
// depends on which line was read first.
func allocationSites(lines []string) map[string]string {
	placements := make(map[string]map[string]bool)
	for _, line := range lines {
		cut := strings.LastIndex(line, "\t")
		if cut < 0 {
			continue
		}
		site, placement := line[:cut], line[cut+1:]
		if placements[site] == nil {
			placements[site] = make(map[string]bool)
		}
		placements[site][placement] = true
	}
	sites := make(map[string]string, len(placements))
	for site, seen := range placements {
		names := make([]string, 0, len(seen))
		for placement := range seen {
			names = append(names, placement)
		}
		sort.Strings(names)
		sites[site] = strings.Join(names, "+")
	}
	return sites
}

// placedIn reports whether a site's placement value -- "frame", "heap" or
// "frame+heap" -- includes one placement.
func placedIn(placement, one string) bool {
	return placement == one || strings.HasPrefix(placement, one+"+") || strings.HasSuffix(placement, "+"+one)
}

func sortedSiteNames(sites map[string]string) []string {
	names := make([]string, 0, len(sites))
	for site := range sites {
		names = append(names, site)
	}
	sort.Strings(names)
	return names
}

func sortedSet(set map[string]bool) []string {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

const allocCensusBaselineHeader = `# Where every allocation in the corpus lands, one per line, as
# opt.AllocationCensus reports it:
#
#     source position <TAB> function <TAB> allocator <TAB> type <TAB> frame|heap
#
# The first four fields are the site's identity and the last is the decision, so
# an object moving between the frame and the heap changes one field of one line
# rather than removing a line and adding another.
#
# Positions are relative to the repository root, and a site the front end gave no
# position is written "?". The function is the one containing the allocation
# after inlining, so a constructor inlined into three callers is three sites --
# which is correct, since each copy is decided separately. The type is the
# type-descriptor symbol with the compiler's prefix and content hash removed, and
# is truncated at 48 characters by the symbol mangler.
#
# The census covers every heap allocation and every frame allocation that came
# out of an escape decision. It does not cover ordinary front-end frame slots;
# there are tens of thousands of those per program, they carry no type, and they
# are not decisions. See opt.AllocationCensus for what that does and does not
# leave visible.
#
# This is a record of what the compiler does, not a list of decisions that are
# correct. Regenerate with
#
#     go test ./goc -run TestAllocationCensus -update-alloc-census-baseline
#
# and read the diff. TestAllocationCensus's doc comment lists what to ask about
# it before committing the result; a placement change accepted without an answer
# to those questions is the failure this file exists to prevent.
`

// TestCompareAllocationCensusNamesTheDirection checks the part of this file a
// reviewer actually reads: the failure text. It runs in milliseconds and does
// not need the corpus, so the reporting stays covered even when the census
// itself is not run.
func TestCompareAllocationCensusNamesTheDirection(t *testing.T) {
	const (
		stayed  = "a.go:1:1\tf\truntime.newobject\tT"
		toHeap  = "a.go:2:2\tg\truntime.newobject\tU"
		toFrame = "a.go:3:3\th\truntime.newobject\tV"
		added   = "a.go:4:4\ti\truntime.newobject\tW"
		removed = "a.go:5:5\tj\truntime.newobject\tX"
	)
	accepted := map[string]bool{
		stayed + "\tframe": true,
		toHeap + "\tframe": true,
		toFrame + "\theap": true,
		removed + "\theap": true,
	}
	found := map[string]string{
		stayed + "\tframe":  "one.go",
		toHeap + "\theap":   "two.go",
		toFrame + "\tframe": "three.go",
		added + "\theap":    "four.go",
	}

	movedToFrame, movedToHeap, appeared, vanished := compareAllocationCensus(found, accepted)

	assert.Equal(t, []string{toFrame + "\n      heap -> frame, seen compiling three.go"}, movedToFrame)
	assert.Equal(t, []string{toHeap + "\n      frame -> heap, seen compiling two.go"}, movedToHeap)
	assert.Equal(t, []string{added + "\n      now on the heap, seen compiling four.go"}, appeared)
	assert.Equal(t, []string{removed + "\n      was on the heap, now absent"}, vanished)
}

// A site decided both ways -- one source position inlined into one function
// twice, promoted in one copy and not the other -- must not read as either
// placement on its own, or a later run would call it a move when nothing moved.
// 439 of the corpus's 18,244 sites are like this.
//
// The direction of such a move is which placement the site gained or lost. A
// site that was frame+heap and is now frame lost a heap allocation: some copy of
// it moved into a frame, and that is the correctness-critical direction, however
// little it looks like one.
func TestCompareAllocationCensusReportsASplitSite(t *testing.T) {
	const site = "a.go:1:1\tf\truntime.newobject\tT"
	both := map[string]string{site + "\tframe": "one.go", site + "\theap": "one.go"}

	for _, testCase := range []struct {
		name            string
		accepted        map[string]bool
		found           map[string]string
		toFrame, toHeap string
		seenIn          string
	}{
		{
			name:     "frame gains a heap copy",
			accepted: map[string]bool{site + "\tframe": true},
			found:    both,
			toHeap:   "frame -> frame+heap",
			seenIn:   "several programs",
		},
		{
			name:     "heap gains a frame copy",
			accepted: map[string]bool{site + "\theap": true},
			found:    both,
			toFrame:  "heap -> frame+heap",
			seenIn:   "several programs",
		},
		{
			name:     "split loses its heap copy",
			accepted: map[string]bool{site + "\tframe": true, site + "\theap": true},
			found:    map[string]string{site + "\tframe": "one.go"},
			toFrame:  "frame+heap -> frame",
			seenIn:   "one.go",
		},
		{
			name:     "split loses its frame copy",
			accepted: map[string]bool{site + "\tframe": true, site + "\theap": true},
			found:    map[string]string{site + "\theap": "one.go"},
			toHeap:   "frame+heap -> heap",
			seenIn:   "one.go",
		},
		{
			name:     "split is unchanged",
			accepted: map[string]bool{site + "\tframe": true, site + "\theap": true},
			found:    both,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			movedToFrame, movedToHeap, appeared, vanished := compareAllocationCensus(testCase.found, testCase.accepted)

			assert.Equal(t, expectedMove(site, testCase.toFrame, testCase.seenIn), movedToFrame)
			assert.Equal(t, expectedMove(site, testCase.toHeap, testCase.seenIn), movedToHeap)
			assert.Empty(t, appeared)
			assert.Empty(t, vanished)
		})
	}
}

// expectedMove builds the one reported line a move produces, or nil when the
// case expects no move in that direction. seenIn is the program the reporter
// names; it is "several programs" whenever the site's new placement is a split,
// because no single census line carries a split placement to attribute it to.
func expectedMove(site, direction, seenIn string) []string {
	if direction == "" {
		return nil
	}
	return []string{fmt.Sprintf("%s\n      %s, seen compiling %s", site, direction, seenIn)}
}
