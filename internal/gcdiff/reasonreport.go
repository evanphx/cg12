package gcdiff

import (
	"fmt"
	"sort"
	"strings"
)

// RenderReasons writes the reason differential as the text file that gets
// checked in.
//
// It is a separate file from the placement differential, and separate for the
// same reason JoinReasons is a separate pass: the placement file is a baseline
// several jobs have read and accepted, and a reason comparison that rewrote it
// would make every future diff of it two things at once. goVersion is in the
// header because -m=2's flow vocabulary is markedly less stable across releases
// than -m's placement wording -- it is internal notation, not a documented
// diagnostic -- so a diff between runs on different releases means even less
// here than it does there.
func RenderReasons(header, goVersion string, result Result) string {
	var out strings.Builder
	out.WriteString(header)
	fmt.Fprintf(&out, "host toolchain: %s\n\n", goVersion)

	writeReasonLegend(&out)
	writeReasonCoverage(&out, result.ReasonCoverage)
	writeReasonSummary(&out, result)

	disagreements := result.ReasonDisagreements()
	unambiguous := 0
	for _, line := range disagreements {
		if line.ExplanationsPairUnambiguously() {
			unambiguous++
		}
	}
	fmt.Fprintf(&out, "## AGREE ON PLACEMENT, DISAGREE ON REASON: %d lines\n\n%s\n", len(disagreements), reasonDisagreementHeader)
	fmt.Fprintf(&out, "of those, %d carry exactly one explained allocation on each side, so the two\n", unambiguous)
	fmt.Fprintf(&out, "explanations are about the same object; the other %d carry more than one and the\n", len(disagreements)-unambiguous)
	out.WriteString("pairing is the line, not the allocation. Both are listed; each line's own\n")
	out.WriteString("entries say which it is.\n\n")
	writeReasonPairs(&out, disagreements)
	writeReasonLines(&out, disagreements)

	agreements := result.ReasonAgreementsAcrossPlacements()
	fmt.Fprintf(&out, "## DISAGREE ON PLACEMENT, AGREE ON REASON: %d lines\n\n%s\n", len(agreements), reasonAgreementHeader)
	writeReasonLines(&out, agreements)

	writeOneSided(&out, "PLACEMENT DISAGREES, ONLY goc EXPLAINED", placementDisagreesGocHeader,
		result.ReasonOneSided(ReasonsGocOnly, false), gocReasonKey)
	writeOneSided(&out, "PLACEMENT DISAGREES, ONLY gc EXPLAINED", placementDisagreesGCHeader,
		result.ReasonOneSided(ReasonsGCOnly, false), gcReasonKey)
	writeOneSided(&out, "PLACEMENT AGREES, ONLY goc EXPLAINED", placementAgreesGocHeader,
		result.ReasonOneSided(ReasonsGocOnly, true), gocReasonKey)
	writeOneSided(&out, "PLACEMENT AGREES, ONLY gc EXPLAINED", placementAgreesGCHeader,
		result.ReasonOneSided(ReasonsGCOnly, true), gcReasonKey)

	writeUncategorised(&out, result.ReasonCoverage)
	return out.String()
}

const reasonLegend = `## the categories

Both compilers' reason vocabularies are normalised into these before anything is
compared. The strings are not comparable -- goc says "argument 0 of
$runtime.newproc escapes" where gc says "from func literal (spill)" under
"flow: {heap} <- &{storage for func literal}" -- and gc's wording is internal
notation that moves between releases. What is comparable is the mechanism by
which the object outlives its frame, which is what a category names.

  call-retains       handed to a call that may keep it
  stored-in-object   written into storage that is not a frame slot: a
                     package-level variable, a field of another object, an
                     element of a container
  interface-boxed    converted to an interface; the payload needs storage
  closure-captured   captured by a closure, a goroutine or a defer
  returned           leaves through one of the function's results
  channel-send       sent on a channel
  read-out           the object's contents are read back out of the container
  too-large          will not fit in a frame                       (gc only)
  loop-carried       one frame slot cannot hold one object per
                     iteration                                    (goc only)
  folded             the analysis stopped deciding this allocation on its own
                     and folded it in with others                 (goc only)
  call-opaque        handed to a call the analysis could not see through: no
                     body in this compilation, no declaration, an unresolved
                     indirect target, or a recursion cut by answering
                     "escapes"                                    (goc only)
  unexplained        the analysis reached a use it could not name (goc only)
  uncategorised      a reason string neither classifier recognised. A gap in
                     this instrument, not a property of the program

The four goc-only categories are goc-only structurally, not incidentally. gc's
escape analysis is complete over the package it compiles plus the summaries in
export data, so it never has to answer "I could not tell": where it cannot see,
it has a fact. A line whose goc category is call-opaque or unexplained is a line
where goc reached gc's answer without gc's knowledge, and a line whose goc
category is loop-carried or folded is a line where goc reached it by a blanket
rule of its own.

goc's three runtime lowerings are translated back to the construct they
implement before categorising -- an argument of $runtime.newproc or
$runtime.deferproc is closure-captured, an argument of $runtime.chansend is
channel-send -- because goc's escape analysis runs after those constructs have
become calls and gc's runs before. That closed list of three is the only
translation in the table that is not a lookup.

An empty category means no explanation, which for both compilers is every frame
placement: a frame placement is the absence of a publication rather than the
presence of a rule, and neither compiler says anything about one. That is what
confines the comparison to lines both compilers put on the heap.

`

func writeReasonLegend(out *strings.Builder) { out.WriteString(reasonLegend) }

func writeReasonCoverage(out *strings.Builder, coverage ReasonCoverage) {
	out.WriteString("## coverage\n\n")
	fmt.Fprintf(out, "programs with reasons on both sides         %6d\n", coverage.Programs)
	fmt.Fprintf(out, "goc -m decisions at joinable positions      %6d\n", coverage.GocSites)
	fmt.Fprintf(out, "  of those on the heap                      %6d\n", coverage.GocHeapSites)
	fmt.Fprintf(out, "  of those carrying a rule                  %6d  (must equal the line above)\n", coverage.GocRules)
	fmt.Fprintf(out, "  of those goc could not explain            %6d  (the \"unexplained\" category: goc saw the\n", coverage.GocUnexplained)
	out.WriteString("                                                    object escape and could not name the use)\n")
	fmt.Fprintf(out, "goc -m decisions with no source position    %6d  (never joinable; so is the census)\n", coverage.GocPositionless)
	fmt.Fprintf(out, "gc -m=2 explained blocks parsed             %6d\n", coverage.GCExplained)
	fmt.Fprintf(out, "  distinct decisions they describe          %6d  (gc explains an allocation once per\n", coverage.GCExplainedDistinct)
	out.WriteString("                                                    function it was inlined into)\n")
	fmt.Fprintf(out, "    reaching a joined line                  %6d\n", coverage.GCExplainedJoined)
	fmt.Fprintf(out, "    for a decision the matrix excludes      %6d  (printed at an inlining call site; see\n", coverage.GCExplainedForExcludedDecision)
	out.WriteString("                                                    package gcdiff. Not a gap)\n")
	fmt.Fprintf(out, "    matching no -m decision at all          %6d  (a gc heap allocation the placement\n", coverage.GCExplainedUnmatched)
	out.WriteString("                                                    differential cannot see; listed below)\n")
	fmt.Fprintf(out, "gc decisions carrying an explanation        %6d\n", coverage.GCDecisionsWithFlow)
	fmt.Fprintf(out, "gc heap decisions with no -m=2 explanation  %6d\n", coverage.GCHeapDecisionsWithoutFlow)
	fmt.Fprintf(out, "  of those synthesized by the chan rule     %6d  (expected: gc prints nothing at all\n", coverage.GCSynthesizedWithoutFlow)
	out.WriteString("                                                    for make(chan))\n")
	fmt.Fprintf(out, "goc rules the classifier did not know       %6d  (must be 0)\n", len(coverage.UncategorisedGocRules))
	fmt.Fprintf(out, "gc flows the classifier did not know        %6d  (must be 0)\n", len(coverage.UncategorisedGCFlows))
	fmt.Fprintf(out, "goc -m lines the parser did not know        %6d  (must be 0)\n", len(coverage.UnknownGocLines))
	out.WriteString("\n")
	out.WriteString("the two instruments on goc's side, against each other:\n\n")
	fmt.Fprintf(out, "lines both record, and agree about          %6d\n", coverage.GocLinesAgree)
	fmt.Fprintf(out, "lines only the census records              %6d  (an allocation goc's own -m cannot\n", coverage.GocLinesCensusOnly)
	out.WriteString("                                                    explain at all)\n")
	fmt.Fprintf(out, "lines only goc -m records                  %6d  (expected: the census omits ordinary\n", coverage.GocLinesDiagnosticOnly)
	out.WriteString("                                                    front-end frame slots)\n")
	fmt.Fprintf(out, "lines one records a subset of the other   %6d  (the same scope difference, arriving on\n", coverage.GocLinesPartial)
	out.WriteString("                                                    a line where the two also overlap)\n")
	fmt.Fprintf(out, "lines both record and contradict          %6d  (neither instrument's placements contain\n", coverage.GocLinesContradicting)
	out.WriteString("                                                    the other's: this one means the tree\n")
	out.WriteString("                                                    moved under the committed census)\n")
	fmt.Fprintf(out, "heap lines goc -m gives no rule for        %6d  (the blind spot: a heap allocation this\n", coverage.GocHeapLinesWithNoRule)
	out.WriteString("                                                    file cannot speak for. They are listed\n")
	out.WriteString("                                                    below; in the committed run all of them\n")
	out.WriteString("                                                    are make(chan), make(map) or make([]T),\n")
	out.WriteString("                                                    allocators goc never considers for a\n")
	out.WriteString("                                                    frame -- so the placement is not a\n")
	out.WriteString("                                                    decision and there is no rule to print.\n")
	out.WriteString("                                                    gc is the same about channels, which is\n")
	out.WriteString("                                                    why this join synthesizes them)\n")
	out.WriteString("\n")
	for _, partial := range coverage.GocLinesPartialList {
		fmt.Fprintf(out, "  SUBSET: %s\n", partial)
	}
	if len(coverage.GocLinesPartialList) > 0 {
		out.WriteString("\n")
	}
	for _, conflict := range coverage.GocLinesContradictingList {
		fmt.Fprintf(out, "  CONTRADICTION: %s\n", conflict)
	}
	if len(coverage.GocLinesContradictingList) > 0 {
		out.WriteString("\n")
	}
	for _, line := range coverage.GocHeapLinesWithNoRuleList {
		fmt.Fprintf(out, "  NO RULE: %s\n", line)
	}
	if len(coverage.GocHeapLinesWithNoRuleList) > 0 {
		out.WriteString("\n")
	}
	if len(coverage.GCExplainedUnmatchedList) > 0 {
		out.WriteString(gcLevelOneBlindSpotHeader)
		for _, entry := range coverage.GCExplainedUnmatchedList {
			fmt.Fprintf(out, "  NOT IN -m: %s\n", entry)
		}
		out.WriteString("\n")
	}
}

const gcLevelOneBlindSpotHeader = `gc heap allocations -m=2 explains and -m never mentions. Two shapes: the closure
a "go" or "defer" statement builds, and the backing array an escaping "append"
reallocates. cmd/compile prints a level-1 verdict for neither, so
escape_gc_differential.txt -- which reads level 1 -- counts gc as having nothing
on these lines, and a line where goc heaps the same object therefore reads
through that join as goc being pessimistic when the two compilers agree. This
list is the extent of it. Fixing it would change the placement differential,
which is deliberately not touched here.

`

func writeReasonSummary(out *strings.Builder, result Result) {
	out.WriteString("## reason agreement, by source line\n\n")
	out.WriteString("rows are whether the two compilers placed the line's allocations the same way,\n")
	out.WriteString("columns what their explanations amount to. \"neither\" is almost entirely lines\n")
	out.WriteString("both compilers keep in a frame, which neither explains.\n\n")

	counts := make(map[[2]bool]map[ReasonComparison]int)
	counts[[2]bool{true, false}] = make(map[ReasonComparison]int)
	counts[[2]bool{false, false}] = make(map[ReasonComparison]int)
	for _, line := range result.Lines {
		if line.GocReasons == nil && line.GCReasons == nil {
			// A line in a program the reason pass did not cover.
			continue
		}
		counts[[2]bool{line.PlacementAgrees(), false}][line.Reasons()]++
	}

	fmt.Fprintf(out, "  %-20s", "placement")
	for _, comparison := range ReasonComparisons {
		fmt.Fprintf(out, "%10s", comparison)
	}
	fmt.Fprintf(out, "%10s\n", "total")
	for _, row := range []struct {
		name   string
		agrees bool
	}{{"agrees", true}, {"disagrees", false}} {
		fmt.Fprintf(out, "  %-20s", row.name)
		total := 0
		for _, comparison := range ReasonComparisons {
			count := counts[[2]bool{row.agrees, false}][comparison]
			total += count
			fmt.Fprintf(out, "%10d", count)
		}
		fmt.Fprintf(out, "%10d\n", total)
	}
	out.WriteString("\n")
}

const reasonDisagreementHeader = `both compilers put something on this line on the heap, and the mechanisms they
name have nothing in common. One of them is reaching the right placement by a
route the other does not recognise, which no placement comparison can see. Read
each one and decide which.
`

const reasonAgreementHeader = `the two compilers place this line differently and still name a common
mechanism. This can only happen on a line carrying more than one allocation:
the compiler that framed says nothing, so the shared category has to come from
something else it heaped on the same line.
`

const placementDisagreesGocHeader = `goc heaps something here that gc does not, so goc is the only compiler with
anything to say. Grouped by goc's category, this is the pessimistic direction
sorted by cause -- the thing that took two jobs of reading goc/compile.go by
hand before the diagnostic existed.
`

const placementDisagreesGCHeader = `gc heaps something here that goc does not, so gc is the only compiler with
anything to say. This is the permissive direction, and gc's category is a direct
statement of what it thinks publishes the object -- which is the fastest triage
available for a line goc may be keeping in a frame wrongly.
`

const placementAgreesGocHeader = `the two compilers agree, and only goc explained. Either gc framed everything it
reported on the line while goc heaped something -- the folded verdicts still
match, which happens on mixed lines -- or gc's -m=2 printed no block for a
decision it made.
`

const placementAgreesGCHeader = `the two compilers agree, and only gc explained. The usual cause is a heap
placement goc made without a rule, or a line where the census records a heap
allocation that goc's -m reports at a different position.
`

// writeReasonPairs summarises a set of lines by which two category sets they
// carry, most common first. It is the answer to "what are these disagreements,
// actually" without reading every line.
func writeReasonPairs(out *strings.Builder, lines []Line) {
	pairs := CountReasons(lines, func(line Line) string {
		return fmt.Sprintf("%-34s %s", line.GocReasons.String(), line.GCReasons.String())
	})
	fmt.Fprintf(out, "by category pair (goc %s gc):\n\n", strings.Repeat(" ", 30))
	for _, pair := range pairs {
		fmt.Fprintf(out, "  %5d  %s\n", pair.Count, pair.Name)
	}
	out.WriteString("\n")
}

func gocReasonKey(line Line) string { return line.GocReasons.String() }
func gcReasonKey(line Line) string  { return line.GCReasons.String() }

func writeOneSided(out *strings.Builder, name, header string, lines []Line, key func(Line) string) {
	fmt.Fprintf(out, "## %s: %d lines\n\n%s\n", name, len(lines), header)
	for _, count := range CountReasons(lines, key) {
		fmt.Fprintf(out, "  %5d  %s\n", count.Count, count.Name)
	}
	out.WriteString("\n")
	writeReasonLines(out, lines)
}

// writeReasonLines prints every line in full: what each compiler placed where,
// in its own words. The categories are what is compared; these are what is read
// when a category is not enough, and the file exists to be read.
func writeReasonLines(out *strings.Builder, lines []Line) {
	for _, line := range lines {
		fmt.Fprintf(out, "%s:%d\t%s -> %s\t%s -> %s\n",
			line.File, line.Number, line.Goc, line.Gc, line.GocReasons.String(), line.GCReasons.String())
		if line.Source != "" {
			fmt.Fprintf(out, "\tsrc  %s\n", line.Source)
		}
		for _, site := range line.GocSites {
			fmt.Fprintf(out, "\tgoc  col %d  %s  %s  [%s: %s in %s]\n",
				site.Col, site.Placement, site.Subject, site.Placer, site.Site, site.Func)
			if site.Rule != "" {
				fmt.Fprintf(out, "\t     %-16s %s\n", site.Reason, site.Rule)
			}
			if site.Use != "" {
				// The position of the use that decided, which is the thing gc's
				// flow chain also names. Printed so the two explanations can be
				// checked against each other rather than only counted.
				fmt.Fprintf(out, "\t     %-16s %s\n", "at", site.Use)
			}
		}
		for _, decision := range line.GC {
			note := ""
			if decision.Synthesized {
				note = "  (synthesized: " + SynthesizeChannelHeap + ")"
			}
			fmt.Fprintf(out, "\tgc   col %d  %s  %s  %s%s\n", decision.Col, decision.Placement, decision.Kind, decision.Subject, note)
			if decision.Flow == nil {
				continue
			}
			fmt.Fprintf(out, "\t     %-16s %s <- %s via %s\n", decision.Reason,
				decision.Flow.Dest, decision.Flow.Source, joinEdges(decision.Flow.Edges))
		}
	}
	out.WriteString("\n")
}

func joinEdges(edges []string) string {
	if len(edges) == 0 {
		return "(no edges)"
	}
	return strings.Join(edges, " -> ")
}

func writeUncategorised(out *strings.Builder, coverage ReasonCoverage) {
	total := len(coverage.UncategorisedGocRules) + len(coverage.UncategorisedGCFlows) + len(coverage.UnknownGocLines)
	fmt.Fprintf(out, "## UNCATEGORISED: %d\n\n%s\n", total, uncategorisedHeader)
	writeUncategorisedGroup(out, "goc rules the classifier did not recognise", coverage.UncategorisedGocRules)
	writeUncategorisedGroup(out, "gc flows the classifier did not recognise", coverage.UncategorisedGCFlows)
	writeUncategorisedGroup(out, "goc -m lines the parser could not read", coverage.UnknownGocLines)
}

const uncategorisedHeader = `an unparsed reason is a gap in this instrument, not the absence of a finding: a
reason that falls out of the taxonomy removes a line from the comparison and
makes the two compilers look as though they agreed about it. Every one is listed
here with its position, and the test fails while any exist.
`

func writeUncategorisedGroup(out *strings.Builder, name string, entries []string) {
	fmt.Fprintf(out, "%s: %d\n", name, len(entries))
	shown := entries
	sort.Strings(shown)
	for _, entry := range shown {
		fmt.Fprintf(out, "  %s\n", entry)
	}
	out.WriteString("\n")
}
