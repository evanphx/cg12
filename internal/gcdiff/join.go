package gcdiff

import (
	"fmt"
	"sort"
	"strings"
)

// Verdict is what one compiler decided about one source line, after every
// decision it made on that line is folded together.
type Verdict string

const (
	// Absent means the compiler recorded no allocation on the line at all.
	//
	// It is not the same as "framed it", and the difference is the largest
	// caveat on this whole measurement. On the goc side the census records
	// every heap allocation but only those frame placements that came out of an
	// escape decision, so a local goc never even considered a candidate is
	// Absent rather than Frame. On the gc side -m prints nothing for an
	// ordinary local it never had to think about. Absent therefore means "not a
	// decision either compiler reported", which usually but not always means
	// the object is in the frame.
	Absent Verdict = "absent"
	// VerdictFrame means every decision on the line was Frame.
	VerdictFrame Verdict = "frame"
	// VerdictHeap means every decision on the line was Heap.
	VerdictHeap Verdict = "heap"
	// VerdictMixed means the line carries both, which happens when more than
	// one allocation is written on it. Mixed lines are reported in their own
	// row rather than folded into either direction, because folding them would
	// be a guess about which allocation on the line is which.
	VerdictMixed Verdict = "mixed"
)

// Verdicts is the order rows and columns of a Matrix are printed in.
var Verdicts = []Verdict{VerdictFrame, VerdictHeap, VerdictMixed, Absent}

// Line is one joined source line: everything both compilers decided about the
// allocations written on it.
type Line struct {
	// File is the corpus program's base name and Number the source line.
	File   string
	Number int
	// Census is every census row goc recorded on the line.
	Census []CensusRow
	// GC is every decision -m printed for the line, including the ones marked
	// Inlined when Join was asked to keep them.
	GC []GCDecision
	// Goc and Gc are the folded verdicts.
	Goc Verdict
	Gc  Verdict
	// Source is the line's text, for triage. Empty when the caller did not
	// supply the program's source.
	Source string
}

// Key is the line's identity, for sorting and for a stable output file.
func (line Line) Key() string {
	return fmt.Sprintf("%s:%d", line.File, line.Number)
}

// Cell names the matrix cell the line falls in.
func (line Line) Cell() [2]Verdict {
	return [2]Verdict{line.Goc, line.Gc}
}

// Matrix counts joined lines per (goc verdict, gc verdict) cell.
type Matrix map[[2]Verdict]int

// Total is how many lines the matrix counts.
func (matrix Matrix) Total() int {
	total := 0
	for _, count := range matrix {
		total += count
	}
	return total
}

// Options controls the parts of the join that are judgement calls rather than
// arithmetic. The zero value is the configuration the checked-in output was
// produced with.
type Options struct {
	// KeepInlinedGCDecisions keeps the -m decisions printed at an inlining call
	// site. They describe copies of allocations written in another file, which
	// goc records under that other file and this join therefore cannot see, so
	// they are excluded from the headline matrix. They are worth keeping for
	// triage of the permissive direction, where the cost of missing a real
	// finding is higher than the cost of looking at a spurious one.
	KeepInlinedGCDecisions bool
	// NoSynthesizedChannels turns off the rule described at
	// SynthesizeChannelHeap.
	NoSynthesizedChannels bool
}

// SynthesizeChannelHeap documents the one gc decision this package supplies
// rather than reads.
//
// cmd/compile prints no -m diagnostic for `make(chan T)` in any form, and has
// no stack-allocation path for a channel: on go1.26.1, `make(chan int, 1)` in a
// function that provably does not let the channel escape still compiles to a
// call to runtime.makechan, which `go build -gcflags=-S` shows directly. So gc
// heap-allocates every channel, unconditionally.
//
// Without this rule every one of goc's 135 comparable `make(chan)` sites would
// join against nothing and be counted as an allocation goc has and gc does not,
// which is exactly backwards: the two compilers agree completely about
// channels. Join therefore adds a synthesized Heap decision for a channel line
// gc said nothing about, and marks it Synthesized so the output file can say
// how many cells rest on the rule rather than on a printed diagnostic.
const SynthesizeChannelHeap = "channels are always heap-allocated by cmd/compile"

// Result is a whole differential run.
type Result struct {
	// Lines is every joined line, sorted by Key.
	Lines []Line
	// Matrix counts them.
	Matrix Matrix
	// Coverage says how much of the corpus the numbers rest on.
	Coverage Coverage
}

// Coverage is the denominator, stated rather than implied.
type Coverage struct {
	// Programs is how many corpus programs exist.
	Programs int
	// Compared is how many of them the host toolchain built, so that both sides
	// of the join exist.
	Compared int
	// Failures is the programs the host toolchain could not build, with the
	// reason, sorted. Their census rows are excluded from the join and counted
	// in ExcludedCensusRows, so an unbuildable program never silently shrinks
	// the denominator.
	Failures []Failure
	// CensusRows is comparable census rows in compared programs.
	CensusRows int
	// ExcludedCensusRows is census rows lost to a program the host toolchain
	// could not build.
	ExcludedCensusRows int
	// CensusDrops is what ParseCensus dropped before any of this.
	CensusDrops CensusDrops
	// GCDecisions is placement decisions parsed from -m across compared
	// programs, after Inlined and Foreign exclusions.
	GCDecisions int
	// InlinedGCDecisions is the decisions excluded because they were printed at
	// an inlining call site.
	InlinedGCDecisions int
	// ForeignGCDiagnostics is diagnostics -m printed at a position outside the
	// corpus program.
	ForeignGCDiagnostics int
	// SynthesizedChannelDecisions is how many gc decisions came from
	// SynthesizeChannelHeap rather than from a printed diagnostic.
	SynthesizedChannelDecisions int
	// UnknownGCDiagnostics is every -m message the parser did not recognise. It
	// must be empty for the run to mean anything.
	UnknownGCDiagnostics []string
}

// Failure is a corpus program the host toolchain would not build.
type Failure struct {
	Program string
	// Reason is the first line of the build error, classified by Classify.
	Reason string
	// CensusRows is how many comparable census rows the program had, so the
	// cost of the exclusion is visible.
	CensusRows int
}

// Program is one corpus program's input to the join.
type Program struct {
	// Name is the base name, e.g. "hello.go".
	Name string
	// Report is what ParseGCFlagM got out of the host build, or nil if the host
	// toolchain could not build the program.
	Report *GCReport
	// BuildError is the reason it could not, when Report is nil.
	BuildError string
	// Source is the program's text, split into lines, for triage output. May be
	// nil.
	Source []string
}

// Join joins the census against the gc reports and returns the matrix.
//
// Programs must list every corpus program, including the ones the host
// toolchain could not build: those contribute to Coverage.Failures and their
// census rows to Coverage.ExcludedCensusRows, which is what keeps an
// unbuildable program from quietly shrinking the denominator.
func Join(census []CensusRow, drops CensusDrops, programs []Program, options Options) Result {
	byName := make(map[string]*Program, len(programs))
	for index := range programs {
		byName[programs[index].Name] = &programs[index]
	}

	result := Result{Matrix: make(Matrix)}
	result.Coverage.Programs = len(programs)
	result.Coverage.CensusDrops = drops

	censusByLine := make(map[string][]CensusRow)
	excludedRows := make(map[string]int)
	for _, row := range census {
		program, known := byName[row.File]
		if !known {
			// A census position naming a corpus file the caller did not list.
			// Treated as excluded rather than dropped silently.
			excludedRows[row.File]++
			continue
		}
		if program.Report == nil {
			excludedRows[row.File]++
			continue
		}
		result.Coverage.CensusRows++
		key := fmt.Sprintf("%s:%d", row.File, row.Line)
		censusByLine[key] = append(censusByLine[key], row)
	}

	gcByLine := make(map[string][]GCDecision)
	for _, program := range programs {
		if program.Report == nil {
			result.Coverage.Failures = append(result.Coverage.Failures, Failure{
				Program:    program.Name,
				Reason:     program.BuildError,
				CensusRows: excludedRows[program.Name],
			})
			result.Coverage.ExcludedCensusRows += excludedRows[program.Name]
			continue
		}
		result.Coverage.Compared++
		result.Coverage.ForeignGCDiagnostics += program.Report.Foreign
		result.Coverage.UnknownGCDiagnostics = append(result.Coverage.UnknownGCDiagnostics, program.Report.Unknown...)
		for _, decision := range program.Report.Decisions {
			if decision.Inlined {
				result.Coverage.InlinedGCDecisions++
				if !options.KeepInlinedGCDecisions {
					continue
				}
			}
			result.Coverage.GCDecisions++
			key := fmt.Sprintf("%s:%d", decision.File, decision.Line)
			gcByLine[key] = append(gcByLine[key], decision)
		}
	}
	sort.Slice(result.Coverage.Failures, func(i, j int) bool {
		return result.Coverage.Failures[i].Program < result.Coverage.Failures[j].Program
	})

	if !options.NoSynthesizedChannels {
		for key, rows := range censusByLine {
			if !hasKind(rows, KindChan) || hasGCKind(gcByLine[key], KindChan) {
				continue
			}
			file, number := splitKey(key)
			gcByLine[key] = append(gcByLine[key], GCDecision{
				File:        file,
				Line:        number,
				Subject:     "make(chan ...)",
				Placement:   Heap,
				Kind:        KindChan,
				Synthesized: true,
			})
			result.Coverage.SynthesizedChannelDecisions++
			result.Coverage.GCDecisions++
		}
	}

	keys := make(map[string]bool, len(censusByLine)+len(gcByLine))
	for key := range censusByLine {
		keys[key] = true
	}
	for key := range gcByLine {
		keys[key] = true
	}
	for key := range keys {
		file, number := splitKey(key)
		line := Line{
			File:   file,
			Number: number,
			Census: censusByLine[key],
			GC:     gcByLine[key],
			Goc:    censusVerdict(censusByLine[key]),
			Gc:     gcVerdict(gcByLine[key]),
		}
		if program := byName[file]; program != nil && number-1 < len(program.Source) && number >= 1 {
			line.Source = strings.TrimSpace(program.Source[number-1])
		}
		sort.Slice(line.Census, func(i, j int) bool { return line.Census[i].Col < line.Census[j].Col })
		sort.Slice(line.GC, func(i, j int) bool { return line.GC[i].Col < line.GC[j].Col })
		result.Lines = append(result.Lines, line)
		result.Matrix[line.Cell()]++
	}
	sort.Slice(result.Lines, func(i, j int) bool {
		if result.Lines[i].File != result.Lines[j].File {
			return result.Lines[i].File < result.Lines[j].File
		}
		return result.Lines[i].Number < result.Lines[j].Number
	})
	sort.Strings(result.Coverage.UnknownGCDiagnostics)
	return result
}

func splitKey(key string) (file string, number int) {
	cut := strings.LastIndex(key, ":")
	file = key[:cut]
	fmt.Sscanf(key[cut+1:], "%d", &number)
	return file, number
}

func hasKind(rows []CensusRow, kind Kind) bool {
	for _, row := range rows {
		if row.Kind() == kind {
			return true
		}
	}
	return false
}

func hasGCKind(decisions []GCDecision, kind Kind) bool {
	for _, decision := range decisions {
		if decision.Kind == kind {
			return true
		}
	}
	return false
}

func censusVerdict(rows []CensusRow) Verdict {
	frame, heap := false, false
	for _, row := range rows {
		switch row.Placement {
		case Frame:
			frame = true
		case Heap:
			heap = true
		}
	}
	return fold(frame, heap)
}

func gcVerdict(decisions []GCDecision) Verdict {
	frame, heap := false, false
	for _, decision := range decisions {
		switch decision.Placement {
		case Frame:
			frame = true
		case Heap:
			heap = true
		}
	}
	return fold(frame, heap)
}

func fold(frame, heap bool) Verdict {
	switch {
	case frame && heap:
		return VerdictMixed
	case frame:
		return VerdictFrame
	case heap:
		return VerdictHeap
	default:
		return Absent
	}
}

// Permissive is every line where gc heaps something and goc does not heap
// everything: the direction where goc is the more dangerous compiler.
//
// It is deliberately wider than the single matrix cell (frame, heap). A line
// where goc's census says nothing at all is included, because the census does
// not record ordinary front-end frame slots -- so "goc has no record here" is
// consistent with "goc put it in the frame without ever calling it a
// candidate", which is the same hazard with less evidence. A line where goc is
// Mixed is included for the same reason. Narrowing to the one cell would report
// a reassuring number that the census cannot support.
func (result Result) Permissive() []Line {
	var lines []Line
	for _, line := range result.Lines {
		if line.Gc != VerdictHeap && line.Gc != VerdictMixed {
			continue
		}
		if line.Goc == VerdictHeap {
			continue
		}
		if !anyHeap(line.GC) {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// Pessimistic is every line where goc heaps something gc does not: the
// direction that costs performance and nothing else.
func (result Result) Pessimistic() []Line {
	var lines []Line
	for _, line := range result.Lines {
		if line.Goc != VerdictHeap && line.Goc != VerdictMixed {
			continue
		}
		if line.Gc == VerdictHeap {
			continue
		}
		if !anyHeapCensus(line.Census) {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func anyHeap(decisions []GCDecision) bool {
	for _, decision := range decisions {
		if decision.Placement == Heap {
			return true
		}
	}
	return false
}

func anyHeapCensus(rows []CensusRow) bool {
	for _, row := range rows {
		if row.Placement == Heap {
			return true
		}
	}
	return false
}
