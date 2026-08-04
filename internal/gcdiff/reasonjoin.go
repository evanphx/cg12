package gcdiff

import (
	"fmt"
	"sort"
	"strings"
)

// Joining the reasons on top of the placements.
//
// # What this adds, and what it deliberately does not touch
//
// The placement join is unchanged: goc's side of it is still the committed
// allocation census and gc's side is still ParseGCFlagM's reading of -m. This
// adds a second pair of inputs -- goc's own -m report and cmd/compile's -m=2
// explanations -- and attaches them to the lines the placement join already
// produced. Every count the placement differential prints comes out of the same
// arithmetic it came out of before.
//
// # Why the interesting cell is heap/heap
//
// Neither compiler explains a frame placement. gc prints `x does not escape`
// and stops; goc prints the same and stops, deliberately and for the same
// stated reason -- a frame placement is the absence of a publication rather
// than the presence of a rule. So a line where both compilers frame the object
// has two empty explanations, which is agreement about nothing.
//
// That leaves the lines where both compilers put something on the heap as the
// only ones where two explanations exist to be compared, and it is where the
// question has teeth: two compilers reaching the same placement by different
// mechanisms means at least one of them is right for a reason that is not the
// reason. ReasonDisagreements is that set.
//
// # And why the reverse question has almost no answers
//
// "Disagree on placement, agree on reason" is nearly empty by construction, not
// by measurement: on a line where one compiler frames and the other heaps, the
// framing compiler has said nothing at all, so there is no reason of its own to
// agree with. It is only expressible on a line carrying more than one
// allocation, where the framing compiler heaped something *else* on the same
// line. ReasonAgreementsAcrossPlacements counts exactly those, and the useful
// form of the question -- what does the compiler that heaped say, on the lines
// where the other one did not -- is PlacementDisagreementReasons.

// ReasonComparison is what one line's two explanation sets amount to.
type ReasonComparison string

const (
	// ReasonsAgree means the two sets share at least one category.
	ReasonsAgree ReasonComparison = "agree"
	// ReasonsDiffer means both compilers explained something on this line and
	// the two sets have nothing in common.
	ReasonsDiffer ReasonComparison = "differ"
	// ReasonsGocOnly means goc explained a heap placement here and gc did not:
	// either gc framed everything on the line, or gc's -m=2 printed no explained
	// block for the decision it made.
	ReasonsGocOnly ReasonComparison = "goc only"
	// ReasonsGCOnly is the mirror.
	ReasonsGCOnly ReasonComparison = "gc only"
	// ReasonsNeither means no explanation on either side, which is every line
	// where both compilers stayed in the frame.
	ReasonsNeither ReasonComparison = "neither"
)

// ReasonComparisons is the order the comparison summary prints in.
var ReasonComparisons = []ReasonComparison{ReasonsAgree, ReasonsDiffer, ReasonsGocOnly, ReasonsGCOnly, ReasonsNeither}

// Reasons classifies the line's two explanation sets.
func (line Line) Reasons() ReasonComparison {
	switch {
	case len(line.GocReasons) == 0 && len(line.GCReasons) == 0:
		return ReasonsNeither
	case len(line.GCReasons) == 0:
		return ReasonsGocOnly
	case len(line.GocReasons) == 0:
		return ReasonsGCOnly
	case line.GocReasons.Intersects(line.GCReasons):
		return ReasonsAgree
	}
	return ReasonsDiffer
}

// PlacementAgrees reports whether the two compilers reached the same verdict for
// the line. It is deliberately the whole verdict and not just "both heap": a
// line both compilers call Mixed is a line they agree about as far as this join
// can see.
func (line Line) PlacementAgrees() bool { return line.Goc == line.Gc }

// ExplanationsPairUnambiguously reports whether the line carries exactly one
// explained allocation on each side.
//
// It matters because the join key is the line and a line can carry several
// allocations. Where it does, "goc's reason" and "gc's reason" can be reasons
// for two different objects, and a disagreement between them is an artefact of
// the key rather than a disagreement about anything. Where each side explained
// exactly one thing, the two explanations are about the same allocation unless
// one compiler missed an allocation the other found -- which is a much smaller
// residue, and one the placement matrix would already have flagged.
//
// Reported alongside the headline count rather than used to filter it: dropping
// the ambiguous lines would understate the disagreement, and keeping them
// unmarked would overstate it.
func (line Line) ExplanationsPairUnambiguously() bool {
	goc := 0
	for _, site := range line.GocSites {
		if site.Placement == Heap && site.Rule != "" {
			goc++
		}
	}
	gc := 0
	for _, decision := range line.GC {
		if decision.Flow != nil {
			gc++
		}
	}
	return goc == 1 && gc == 1
}

// ReasonPair is the line's two explanation sets rendered as one key, for
// counting which disagreements are common.
func (line Line) ReasonPair() [2]string {
	return [2]string{line.GocReasons.String(), line.GCReasons.String()}
}

// ReasonCoverage is how much of the reason comparison rests on what.
//
// Every field is a way the instrument can be less complete than it looks, and
// each is printed rather than folded into a denominator.
type ReasonCoverage struct {
	// Programs is how many programs contributed reasons on both sides.
	Programs int
	// GocSites is decisions read out of goc's -m at joinable positions, and
	// GocHeapSites the subset on the heap -- the ones that can carry a rule.
	GocSites     int
	GocHeapSites int
	// GocRules is how many of those heap sites carried a rule at all.
	GocRules int
	// GocPositionless is decisions goc's -m printed with no source position, so
	// they can never join. The census has the same blind spot.
	GocPositionless int
	// GocUnexplained is the subset of GocRules whose category is
	// ReasonUnexplained: goc watched the object escape and could not name the
	// use. It is the honest denominator for everything else in this file, since
	// a line goc did not explain cannot disagree with gc about anything.
	GocUnexplained int
	// The two instruments on goc's side do not record the same set of
	// allocations, and saying how they differ is the only way the reason side
	// and the placement side can be known to be talking about the same compiler.
	// The census is committed and the -m report is compiled fresh, so a
	// contradiction between them means the tree moved under the census; the two
	// one-sided counts are scope differences and are expected.
	//
	// GocLinesAgree is lines both record and agree about.
	GocLinesAgree int
	// GocLinesCensusOnly is lines the census records a placement on and goc's -m
	// reports nothing for. Every one is an allocation goc's own diagnostic
	// cannot explain, so it bounds how much of goc's heap this file can speak
	// for -- see GocHeapLinesWithNoRule for the half that matters.
	GocLinesCensusOnly int
	// GocLinesDiagnosticOnly is lines goc's -m reports a placement on and the
	// census does not record. Expected: the census deliberately omits ordinary
	// front-end frame slots, which have no allocator and are not decisions,
	// while -m reports every placement either placer made.
	GocLinesDiagnosticOnly int
	// GocLinesPartial is lines both record where one instrument's placements are
	// a strict subset of the other's -- the census says frame+heap and -m says
	// frame, because the heap half is a make(map) whose placement is not a
	// decision and so has no rule to print. A subset is the same scope
	// difference as the two one-sided counts above, arriving on a line where the
	// two instruments also overlap.
	GocLinesPartial     int
	GocLinesPartialList []string
	// GocLinesContradicting is lines both record where neither instrument's
	// placements contain the other's, so one of them puts an object somewhere
	// the other does not. This is the one that means drift, and the only one
	// asserted on.
	GocLinesContradicting     int
	GocLinesContradictingList []string
	// GocHeapLinesWithNoRule is lines the census puts an allocation on the heap
	// on and goc's -m gives no heap rule for. It is the blind spot that matters:
	// a heap allocation this file cannot say anything about at all.
	GocHeapLinesWithNoRule     int
	GocHeapLinesWithNoRuleList []string
	// GCExplained is explained blocks parsed at joinable positions.
	// GCExplainedDistinct is how many distinct decisions they describe: gc
	// explains an allocation once per function it was inlined into, and the join
	// key deliberately does not carry the function, so the same decision can be
	// explained several times. GCExplainedJoined is how many of those distinct
	// decisions reached a joined line.
	GCExplained         int
	GCExplainedDistinct int
	GCExplainedJoined   int
	// GCDecisionsWithFlow is how many -m decisions ended up carrying an
	// explanation. It exceeds GCExplainedJoined when -m printed one decision
	// twice, which it does for a subject appearing twice on one line.
	GCDecisionsWithFlow int
	// GCExplainedForExcludedDecision is explanations whose decision the
	// placement join excluded from the matrix because it was printed at an
	// inlining call site -- a copy of an allocation written in the host standard
	// library, which the census records under a file this join cannot see. They
	// are not a gap; they are the level-2 half of an exclusion the placement
	// differential already documents and counts.
	GCExplainedForExcludedDecision int
	// GCExplainedUnmatched is explanations for a decision -m never printed at
	// level 1, with GCExplainedUnmatchedList naming them.
	//
	// It is not zero and it is not noise. cmd/compile explains two things at
	// level 2 that it reports no verdict for at level 1: the closure a `go` or
	// `defer` statement builds, and the backing array an escaping `append`
	// reallocates. Both are gc heap allocations, and the placement differential
	// -- which reads level 1 -- cannot see either. Every entry is a gc heap
	// allocation escape_gc_differential.txt counts as gc having nothing on the
	// line, which reads through that join as goc being pessimistic.
	GCExplainedUnmatched     int
	GCExplainedUnmatchedList []string
	// GCHeapDecisionsWithoutFlow is heap decisions from -m that no -m=2 block
	// explained. A synthesized channel decision is always one of these -- gc
	// prints nothing at all for make(chan) -- and they are counted here rather
	// than treated as gc having no reason.
	GCHeapDecisionsWithoutFlow int
	// GCSynthesizedWithoutFlow is the subset of the above that came from
	// SynthesizeChannelHeap, which is expected and is not a gap.
	GCSynthesizedWithoutFlow int
	// UncategorisedGocRules and UncategorisedGCFlows are the reason strings
	// neither classifier recognised, with positions, for reporting. They must be
	// empty for the categories to cover what they claim to cover.
	UncategorisedGocRules []string
	UncategorisedGCFlows  []string
	// UnknownGocLines is every line of goc's -m the parser could not read. Like
	// UnknownGCDiagnostics it must be empty.
	UnknownGocLines []string
}

// ReasonMatrix counts lines per (goc category set, gc category set) pair.
type ReasonMatrix map[[2]string]int

// JoinReasons attaches the reason inputs to an already-joined result.
//
// It is a second pass rather than part of Join for one reason: Join is what
// produces the committed placement differential, and a change to it is a change
// to a file several jobs have read and accepted. This function reads the result
// Join produced and adds to it.
func JoinReasons(result *Result, programs []Program) {
	byName := make(map[string]*Program, len(programs))
	for index := range programs {
		byName[programs[index].Name] = &programs[index]
	}

	gocByLine := make(map[string][]GocSite)
	gocVerdictByLine := make(map[string]Verdict)
	for _, program := range programs {
		if program.Goc == nil || program.Explanations == nil {
			continue
		}
		result.ReasonCoverage.Programs++
		result.ReasonCoverage.GocPositionless += program.Goc.Positionless
		result.ReasonCoverage.UncategorisedGocRules = append(result.ReasonCoverage.UncategorisedGocRules, program.Goc.UncategorisedRules...)
		result.ReasonCoverage.UnknownGocLines = append(result.ReasonCoverage.UnknownGocLines, program.Goc.Unknown...)
		result.ReasonCoverage.GCExplained += program.Explanations.Blocks
		for _, site := range program.Goc.Sites {
			result.ReasonCoverage.GocSites++
			if site.Placement == Heap {
				result.ReasonCoverage.GocHeapSites++
				if site.Rule != "" {
					result.ReasonCoverage.GocRules++
				}
				if site.Reason == ReasonUnexplained {
					result.ReasonCoverage.GocUnexplained++
				}
			}
			key := fmt.Sprintf("%s:%d", site.File, site.Line)
			gocByLine[key] = append(gocByLine[key], site)
		}
		accountExplanations(program, &result.ReasonCoverage)
	}
	for key, sites := range gocByLine {
		// A total order, for the reason opt.EscapeSite.sortKey gives: two sites
		// this cannot tell apart come out in whichever order an unstable sort
		// left them, and the rendered file is meant to be diffed. One position
		// in one function holds both a frame and a heap decision often enough
		// that column and subject alone are not enough.
		sort.Slice(sites, func(i, j int) bool {
			return sites[i].sortKey() < sites[j].sortKey()
		})
		gocByLine[key] = sites
		frame, heap := false, false
		for _, site := range sites {
			switch site.Placement {
			case Frame:
				frame = true
			case Heap:
				heap = true
			}
		}
		gocVerdictByLine[key] = fold(frame, heap)
	}

	result.ReasonMatrix = make(ReasonMatrix)
	for index := range result.Lines {
		line := &result.Lines[index]
		program := byName[line.File]
		if program == nil || program.Goc == nil || program.Explanations == nil {
			continue
		}
		key := line.Key()
		line.GocSites = gocByLine[key]
		line.GocReasons = make(ReasonSet)
		line.GCReasons = make(ReasonSet)

		for _, site := range line.GocSites {
			if site.Placement != Heap || site.Rule == "" {
				continue
			}
			line.GocReasons[site.Reason] = true
		}
		for decision := range line.GC {
			attachGCFlow(&line.GC[decision], program.Explanations, &result.ReasonCoverage)
			if line.GC[decision].Reason != "" {
				line.GCReasons[line.GC[decision].Reason] = true
			}
		}
		diagnostic, recorded := gocVerdictByLine[key]
		if !recorded {
			diagnostic = Absent
		}
		coverage := &result.ReasonCoverage
		switch {
		case diagnostic == Absent && line.Goc == Absent:
		case diagnostic == Absent:
			coverage.GocLinesCensusOnly++
		case line.Goc == Absent:
			coverage.GocLinesDiagnosticOnly++
		case diagnostic == line.Goc:
			coverage.GocLinesAgree++
		case placementsContain(line.Goc, diagnostic) || placementsContain(diagnostic, line.Goc):
			coverage.GocLinesPartial++
			coverage.GocLinesPartialList = append(coverage.GocLinesPartialList,
				fmt.Sprintf("%s: census says %s, goc -m says %s", key, line.Goc, diagnostic))
		default:
			coverage.GocLinesContradicting++
			coverage.GocLinesContradictingList = append(coverage.GocLinesContradictingList,
				fmt.Sprintf("%s: census says %s, goc -m says %s", key, line.Goc, diagnostic))
		}
		if (line.Goc == VerdictHeap || line.Goc == VerdictMixed) && len(line.GocReasons) == 0 {
			coverage.GocHeapLinesWithNoRule++
			coverage.GocHeapLinesWithNoRuleList = append(coverage.GocHeapLinesWithNoRuleList,
				fmt.Sprintf("%s: census says %s, goc -m gives no heap rule", key, line.Goc))
		}
		result.ReasonMatrix[line.ReasonPair()]++
	}
	sort.Strings(result.ReasonCoverage.GocLinesContradictingList)
	sort.Strings(result.ReasonCoverage.GocLinesPartialList)
	sort.Strings(result.ReasonCoverage.GocHeapLinesWithNoRuleList)
	sort.Strings(result.ReasonCoverage.UncategorisedGocRules)
	sort.Strings(result.ReasonCoverage.UncategorisedGCFlows)
	sort.Strings(result.ReasonCoverage.UnknownGocLines)
}

// accountExplanations says where every explained block went, so that the
// difference between "blocks parsed" and "blocks joined" is a set of named
// exclusions rather than a subtraction the reader has to trust.
//
// The bulk of the difference is decisions the placement join excludes because
// they were printed at an inlining call site; those decisions never reach a
// Line, so their explanations never get attached, and that is correct rather
// than lossy. What must not exist is an explanation matching no decision at
// all.
func accountExplanations(program Program, coverage *ReasonCoverage) {
	included := make(map[ExplanationKey]bool)
	excluded := make(map[ExplanationKey]bool)
	for _, decision := range program.Report.Decisions {
		if decision.Placement != Heap {
			continue
		}
		key := ExplanationKey{Line: decision.Line, Col: decision.Col, Subject: decision.Subject}
		if decision.Inlined {
			excluded[key] = true
			continue
		}
		included[key] = true
	}
	coverage.GCExplainedDistinct += len(program.Explanations.Flows)
	for key := range program.Explanations.Flows {
		switch {
		case included[key]:
			coverage.GCExplainedJoined++
		case excluded[key]:
			coverage.GCExplainedForExcludedDecision++
		default:
			coverage.GCExplainedUnmatched++
			coverage.GCExplainedUnmatchedList = append(coverage.GCExplainedUnmatchedList,
				fmt.Sprintf("%s:%d:%d: %s", program.Name, key.Line, key.Col, key.Subject))
		}
	}
	sort.Strings(coverage.GCExplainedUnmatchedList)
}

// attachGCFlow finds the -m=2 explanation for one level-1 decision.
//
// The key is (line, column, subject), which is what both levels print
// identically. A `moved to heap: x` decision is explained by a block headed
// `x escapes to heap in F:` -- the subject the level-1 parse already stripped
// the prefix from -- so the two forms need no special case.
func attachGCFlow(decision *GCDecision, explanations *GCExplanations, coverage *ReasonCoverage) {
	if decision.Placement != Heap {
		// gc explains escapes and nothing else. A frame decision with an
		// explanation attached would be this parser having matched the wrong
		// block.
		return
	}
	flow, found := explanations.Flows[ExplanationKey{Line: decision.Line, Col: decision.Col, Subject: decision.Subject}]
	if !found {
		coverage.GCHeapDecisionsWithoutFlow++
		if decision.Synthesized {
			coverage.GCSynthesizedWithoutFlow++
		}
		return
	}
	coverage.GCDecisionsWithFlow++
	decision.Flow = &flow
	reason, known := flow.Reason()
	decision.Reason = reason
	if !known {
		coverage.UncategorisedGCFlows = append(coverage.UncategorisedGCFlows,
			fmt.Sprintf("%s:%d:%d: %s", decision.File, decision.Line, decision.Col, strings.ReplaceAll(flow.Text, "\n", " | ")))
	}
}

// placementsContain reports whether the placements one verdict stands for
// include every placement the other stands for.
//
// The two instruments on goc's side record different subsets of the same
// allocations -- the census omits ordinary front-end frame slots, and -m omits
// every allocator whose placement was never a decision -- so a line where one
// reports strictly less than the other is a scope difference. Only a line where
// neither contains the other is a placement the two disagree about.
func placementsContain(outer, inner Verdict) bool {
	outerFrame, outerHeap := placementsOf(outer)
	innerFrame, innerHeap := placementsOf(inner)
	return (outerFrame || !innerFrame) && (outerHeap || !innerHeap)
}

func placementsOf(verdict Verdict) (frame, heap bool) {
	switch verdict {
	case VerdictFrame:
		return true, false
	case VerdictHeap:
		return false, true
	case VerdictMixed:
		return true, true
	}
	return false, false
}

// ReasonDisagreements is the deliverable: every line the two compilers place
// the same way and explain incompatibly.
//
// "The same way" is the folded verdict, and in practice every member is a line
// both compilers put on the heap, because a line both compilers frame has no
// explanation on either side to disagree with.
func (result Result) ReasonDisagreements() []Line {
	var lines []Line
	for _, line := range result.Lines {
		if line.PlacementAgrees() && line.Reasons() == ReasonsDiffer {
			lines = append(lines, line)
		}
	}
	return lines
}

// ReasonAgreementsAcrossPlacements is the reverse question: lines the two
// compilers place differently and explain the same way.
//
// See the file comment for why this is nearly empty and what it means when it
// is not: only a line carrying more than one allocation can be in it.
func (result Result) ReasonAgreementsAcrossPlacements() []Line {
	var lines []Line
	for _, line := range result.Lines {
		if !line.PlacementAgrees() && line.Reasons() == ReasonsAgree {
			lines = append(lines, line)
		}
	}
	return lines
}

// ReasonOneSided is every line where exactly one compiler explained something,
// in the given direction. It is the honest form of the reverse question: on a
// line the two compilers place differently, the one that heaped is the only one
// with anything to say, and what it says is the triage.
func (result Result) ReasonOneSided(comparison ReasonComparison, placementAgrees bool) []Line {
	var lines []Line
	for _, line := range result.Lines {
		if line.PlacementAgrees() != placementAgrees {
			continue
		}
		if line.Reasons() != comparison {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// ReasonCount is one category with a count, for a sorted summary.
type ReasonCount struct {
	Name  string
	Count int
}

// CountReasons tallies a set of lines by a key, most common first and ties
// broken by name so the output is stable.
func CountReasons(lines []Line, key func(Line) string) []ReasonCount {
	counts := make(map[string]int)
	for _, line := range lines {
		counts[key(line)]++
	}
	out := make([]ReasonCount, 0, len(counts))
	for name, count := range counts {
		out = append(out, ReasonCount{name, count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}
