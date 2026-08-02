package gcdiff

import (
	"fmt"
	"sort"
	"strings"
)

// Render writes the whole differential as the text file that gets checked in.
//
// The file is meant to be diffed, so everything in it is sorted and nothing in
// it depends on the order programs happened to finish. goVersion is written
// into the header because -m's wording is not stable across releases: a diff
// between two runs of this file means nothing unless both say the same version.
func Render(header, goVersion string, result Result) string {
	var out strings.Builder
	out.WriteString(header)
	fmt.Fprintf(&out, "host toolchain: %s\n\n", goVersion)

	writeCoverage(&out, result.Coverage)
	writeMatrix(&out, result.Matrix)
	writeDirection(&out, "PERMISSIVE", permissiveHeader, result.Permissive())
	writeDirection(&out, "PESSIMISTIC", pessimisticHeader, result.Pessimistic())
	return out.String()
}

func writeCoverage(out *strings.Builder, coverage Coverage) {
	out.WriteString("## coverage\n\n")
	fmt.Fprintf(out, "corpus programs                          %6d\n", coverage.Programs)
	fmt.Fprintf(out, "compared (host toolchain built them)     %6d\n", coverage.Compared)
	fmt.Fprintf(out, "not compared                             %6d\n", len(coverage.Failures))
	fmt.Fprintf(out, "census rows joined                       %6d\n", coverage.CensusRows)
	fmt.Fprintf(out, "census rows lost to an unbuilt program   %6d\n", coverage.ExcludedCensusRows)
	fmt.Fprintf(out, "census rows with no source position      %6d  (never joinable)\n", coverage.CensusDrops.NoPosition)
	fmt.Fprintf(out, "  of those, in a corpus program's own code %5d  (%d on the heap: the bound on how\n", coverage.CensusDrops.NoPositionInProgram, coverage.CensusDrops.NoPositionHeapInProgram)
	out.WriteString("                                                  wrong the join can be about goc)\n")
	fmt.Fprintf(out, "census rows outside the corpus directory %6d  (vendored stdlib; never joinable)\n", coverage.CensusDrops.OtherTree)
	fmt.Fprintf(out, "gc decisions joined                      %6d\n", coverage.GCDecisions)
	fmt.Fprintf(out, "  of which synthesized by the chan rule  %6d\n", coverage.SynthesizedChannelDecisions)
	fmt.Fprintf(out, "gc decisions at an inlining call site    %6d  (excluded; see package gcdiff)\n", coverage.InlinedGCDecisions)
	fmt.Fprintf(out, "gc diagnostics outside the program       %6d  (excluded; wrappers and instantiations)\n", coverage.ForeignGCDiagnostics)
	fmt.Fprintf(out, "gc diagnostics the parser did not know   %6d  (must be 0)\n", len(coverage.UnknownGCDiagnostics))
	out.WriteString("\n")
	if len(coverage.Failures) > 0 {
		out.WriteString("programs the host toolchain would not build, and the census rows each costs:\n\n")
		for _, failure := range coverage.Failures {
			fmt.Fprintf(out, "  %-40s %4d rows  %s\n", failure.Program, failure.CensusRows, failure.Reason)
		}
		out.WriteString("\n")
	}
	for _, unknown := range coverage.UnknownGCDiagnostics {
		fmt.Fprintf(out, "  UNKNOWN -m DIAGNOSTIC: %s\n", unknown)
	}
	if len(coverage.UnknownGCDiagnostics) > 0 {
		out.WriteString("\n")
	}
}

func writeMatrix(out *strings.Builder, matrix Matrix) {
	out.WriteString("## confusion matrix, by source line\n\n")
	out.WriteString("rows are goc's verdict for the line, columns are gc's.\n")
	out.WriteString("\"absent\" is \"this compiler reported no allocation here\", which is not the\n")
	out.WriteString("same as \"it put the object in a frame\" -- see gcdiff.Absent.\n\n")
	fmt.Fprintf(out, "  %-8s", "goc\\gc")
	for _, verdict := range Verdicts {
		fmt.Fprintf(out, "%9s", verdict)
	}
	fmt.Fprintf(out, "%9s\n", "total")
	for _, row := range Verdicts {
		fmt.Fprintf(out, "  %-8s", row)
		total := 0
		for _, column := range Verdicts {
			count := matrix[[2]Verdict{row, column}]
			total += count
			fmt.Fprintf(out, "%9d", count)
		}
		fmt.Fprintf(out, "%9d\n", total)
	}
	fmt.Fprintf(out, "  %-8s", "total")
	for _, column := range Verdicts {
		total := 0
		for _, row := range Verdicts {
			total += matrix[[2]Verdict{row, column}]
		}
		fmt.Fprintf(out, "%9d", total)
	}
	fmt.Fprintf(out, "%9d\n\n", matrix.Total())
}

const permissiveHeader = `every source line gc puts on the heap and goc does not. This is the
correctness-critical direction: goc keeping in a frame something gc could not
prove frame-safe is a candidate for a frame address outliving its frame. Lines
where goc's census says nothing at all are included, because the census does
not record ordinary front-end frame slots, so "no record" is consistent with
"framed without ever being called a candidate".
`

const pessimisticHeader = `every source line goc puts on the heap and gc does not. This is the
performance direction and nothing worse: each line is an allocation, a zeroing
and a collector's attention that the reference implementation does not pay.
`

func writeDirection(out *strings.Builder, name, header string, lines []Line) {
	fmt.Fprintf(out, "## %s: %d lines\n\n%s\n", name, len(lines), header)
	writeClasses(out, lines)
	for _, line := range lines {
		fmt.Fprintf(out, "%s:%d\t%s -> %s\n", line.File, line.Number, line.Goc, line.Gc)
		if line.Source != "" {
			fmt.Fprintf(out, "\tsrc  %s\n", line.Source)
		}
		for _, row := range line.Census {
			fmt.Fprintf(out, "\tgoc  col %d  %s  %s  %s  [%s]\n", row.Col, row.Placement, row.Allocator, row.Type, row.Func)
		}
		for _, decision := range line.GC {
			note := ""
			if decision.Synthesized {
				note = "  (synthesized: " + SynthesizeChannelHeap + ")"
			}
			fmt.Fprintf(out, "\tgc   col %d  %s  %s  %s%s\n", decision.Col, decision.Placement, decision.Kind, decision.Subject, note)
		}
	}
	out.WriteString("\n")
}

// writeClasses summarises a direction by what was being allocated on each side,
// so the top classes are readable without going through every line.
func writeClasses(out *strings.Builder, lines []Line) {
	counts := make(map[string]int)
	for _, line := range lines {
		counts[classOf(line)]++
	}
	type class struct {
		name  string
		count int
	}
	classes := make([]class, 0, len(counts))
	for name, count := range counts {
		classes = append(classes, class{name, count})
	}
	sort.Slice(classes, func(i, j int) bool {
		if classes[i].count != classes[j].count {
			return classes[i].count > classes[j].count
		}
		return classes[i].name < classes[j].name
	})
	out.WriteString("by construct (goc allocators on the line | gc constructs on the line):\n\n")
	for _, entry := range classes {
		fmt.Fprintf(out, "  %5d  %s\n", entry.count, entry.name)
	}
	out.WriteString("\n")
}

func classOf(line Line) string {
	return joinSorted(gocAllocators(line.Census)) + " | " + joinSorted(gcKinds(line.GC))
}

func gocAllocators(rows []CensusRow) []string {
	seen := make(map[string]bool)
	for _, row := range rows {
		seen[strings.TrimPrefix(row.Allocator, "runtime.")+"/"+string(row.Placement)] = true
	}
	return keysOf(seen)
}

func gcKinds(decisions []GCDecision) []string {
	seen := make(map[string]bool)
	for _, decision := range decisions {
		seen[string(decision.Kind)+"/"+string(decision.Placement)] = true
	}
	return keysOf(seen)
}

func keysOf(seen map[string]bool) []string {
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	return out
}

func joinSorted(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}
