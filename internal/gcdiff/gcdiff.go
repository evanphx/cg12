// Package gcdiff joins goc's allocation census against the escape decisions
// cmd/compile prints for -gcflags=-m, so that "is goc's escape analysis as good
// as the Go compiler's" has a number instead of an opinion.
//
// Everything measured about goc's escape analysis before this compared two of
// goc's own analyses to each other: the AST walk against the summary-fed IR
// pass. That answers "which of ours is better", not "are either of them any
// good". The reference implementation is installed on the build machine and
// will print its decisions on request, so this package asks it and reports the
// confusion matrix.
//
// # The comparable universe
//
// goc compiles a vendored standard library out of stdlib/src; the host go
// compiles its own. Those are different source trees, so a position in one does
// not name anything in the other and no join across them is possible. What both
// compilers do read byte-for-byte identically is the corpus program itself, so
// the comparable universe is exactly the allocations whose source text is
// written in goc/testdata/*.go. Everything else is dropped, counted, and
// reported as dropped -- see Coverage.
//
// # The join key, and why it is the source line
//
// The key is (corpus file, source line). Not the column, and not the containing
// function. Both omissions are forced:
//
//   - Columns do not correspond. The two front ends hang an allocating
//     construct off different tokens: for `new(record)` goc records the column
//     of `new` and gc the column of `(`; for a map literal goc records the
//     column of `map` and gc the column of `{`; for `x * 7` boxed into an
//     interface goc records the column of `7` and gc the column of `*`. Over
//     the corpus, exact (line, column) agreement joins 1 559 of 2 607 census
//     positions where the line alone joins 2 115 of 2 473, and the residual
//     column deltas have no single offset that corrects them -- they range from
//     -18 to +17 with no mode. A column-exact join would therefore report
//     hundreds of position artefacts as disagreements.
//
//   - Functions do not correspond, because inlining differs. goc's inliner
//     copies each instruction with the callee's own position (opt/inline.go's
//     cloneInstr keeps in.Pos), so an inlined allocation stays attributed to the
//     line it is written on and lands in the census under the callee's file.
//     cmd/compile does the opposite: -m prints the outermost position, so an
//     inlined allocation is reported at the call site. The two conventions are
//     irreconcilable, and the containing function is not comparable at all.
//
// Dropping both is what makes the join sound rather than lossy: what is being
// compared is "for the allocation written on this line, where does each
// compiler put it", which is a question about the source, and the source is the
// one thing the two compilers share.
//
// The inlining convention mismatch has one further consequence, handled in
// ParseGCFlagM: a gc diagnostic printed at a position that also carries an
// "inlining call to" diagnostic is a copy of an allocation written somewhere
// else -- usually in the host standard library -- attributed to this line. goc
// records that same allocation under stdlib/src, outside the comparable
// universe, so counting the gc copy would invent a disagreement out of an
// allocation goc placed in a file this join cannot see. Those decisions are
// marked Inlined and excluded from the matrix by default; the count is
// reported, and Join can be asked to include them for triage.
//
// # What counts as a site
//
// A line, deduplicated, exactly as the census-count reconciliation resolved it:
// one source position in one function after inlining, deduplicated, then
// aggregated up to the line because the column and the function are not
// portable across compilers. Where more than one allocation is written on one
// line and the two compilers disagree about only some of them, the line's
// verdict is Mixed and it is reported in its own row rather than folded into
// either direction.
package gcdiff

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Placement is where a compiler put an object.
type Placement string

const (
	// Frame means the object lives in the allocating function's frame.
	Frame Placement = "frame"
	// Heap means the object is allocated by the runtime allocator.
	Heap Placement = "heap"
)

// Kind classifies the allocating construct, so that a disagreement can be
// characterised by what was being allocated rather than only counted.
//
// It is deliberately not part of the join key. The two compilers lower the same
// construct to different allocators often enough that keying on it would split
// agreements apart: `make([]byte, aes.BlockSize)` is a makeslice to gc and a
// newobject of a 16-byte array to goc, and those are the same allocation.
type Kind string

const (
	// KindObject is a single object: new(T), &T{}, a boxed value, a closure, a
	// local whose address was taken.
	KindObject Kind = "object"
	// KindSlice is a slice backing array: make([]T, n), []T{...}, a variadic
	// argument slice.
	KindSlice Kind = "slice"
	// KindMap is a map: make(map[K]V) or a map literal.
	KindMap Kind = "map"
	// KindChan is a channel: make(chan T).
	KindChan Kind = "chan"
)

// CensusRow is one line of goc/testdata/alloc_census_baseline.txt.
type CensusRow struct {
	// File is the position's file as the census wrote it, repository-relative.
	File string
	// Line and Col are the position. A census row with no position is dropped
	// by ParseCensus, since it can never join.
	Line int
	Col  int
	// Func is the IR function containing the allocation after inlining. It is
	// recorded for diagnostics and is deliberately not part of the join key;
	// see the package comment.
	Func string
	// Allocator is the runtime allocator a heap placement calls, or that a
	// frame placement avoided calling.
	Allocator string
	// Type is the allocated type as the census names it.
	Type string
	// Placement is where goc put the object.
	Placement Placement
}

// Kind classifies the row by the allocator the census recorded.
func (row CensusRow) Kind() Kind {
	switch row.Allocator {
	case "runtime.makeslice", "runtime.makeslice64":
		return KindSlice
	case "runtime.makemap", "runtime.makemap64", "runtime.makemap_small":
		return KindMap
	case "runtime.makechan", "runtime.makechan64":
		return KindChan
	default:
		return KindObject
	}
}

var censusPosition = regexp.MustCompile(`^([^:]+):(\d+):(\d+)$`)

// ParseCensus reads alloc_census_baseline.txt and returns the rows whose
// position is under dir, with dir stripped from the file name.
//
// Rows outside dir -- the vendored standard library, and the 5 863 rows the
// front end gave no position at all -- cannot join against a host-toolchain
// build of a corpus program and are dropped here. The counts are returned so
// the caller can report the size of what it dropped rather than quietly
// shrinking its denominator.
func ParseCensus(text, dir string) (rows []CensusRow, dropped CensusDrops, err error) {
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	for number, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 5 {
			return nil, dropped, fmt.Errorf("census line %d: want 5 tab-separated fields, got %d: %q", number+1, len(fields), line)
		}
		position, function, allocator, allocated, placement := fields[0], fields[1], fields[2], fields[3], fields[4]
		if placement != string(Frame) && placement != string(Heap) {
			return nil, dropped, fmt.Errorf("census line %d: unknown placement %q", number+1, placement)
		}
		if position == "?" {
			dropped.NoPosition++
			if strings.HasPrefix(function, "main.") {
				dropped.NoPositionInProgram++
				if placement == string(Heap) {
					dropped.NoPositionHeapInProgram++
				}
			}
			continue
		}
		if !strings.HasPrefix(position, dir) {
			dropped.OtherTree++
			continue
		}
		match := censusPosition.FindStringSubmatch(strings.TrimPrefix(position, dir))
		if match == nil {
			return nil, dropped, fmt.Errorf("census line %d: unparseable position %q", number+1, position)
		}
		row := CensusRow{
			File:      match[1],
			Func:      function,
			Allocator: allocator,
			Type:      allocated,
			Placement: Placement(placement),
		}
		row.Line, _ = strconv.Atoi(match[2])
		row.Col, _ = strconv.Atoi(match[3])
		rows = append(rows, row)
	}
	return rows, dropped, nil
}

// CensusDrops counts the census rows that cannot take part in the join, by
// reason. Both reasons are structural rather than incidental: neither a
// positionless row nor a row in the vendored standard library has anything on
// the gc side to be compared against.
type CensusDrops struct {
	// NoPosition is rows the front end gave no source position.
	NoPosition int
	// NoPositionInProgram is the subset of those whose function belongs to the
	// corpus program itself rather than to the vendored standard library.
	//
	// This number is the honest bound on how wrong the position join can be
	// about goc's side. An allocation goc's lowering creates rather than the
	// source asks for carries no position, so it cannot join, and a line can
	// therefore look as though goc allocated nothing on it when goc allocated
	// something anonymous. The per-iteration copy of an address-taken loop
	// variable is exactly that: goc records a frame decision at the loop
	// header's position and a positionless heap allocation for the copy, which
	// reads through the join as "goc framed what gc heaps" and is not.
	NoPositionInProgram int
	// NoPositionHeapInProgram is the subset of NoPositionInProgram that is on
	// the heap, which is the direction that can explain away a permissive
	// finding.
	NoPositionHeapInProgram int
	// OtherTree is rows positioned outside the corpus directory -- almost all
	// of them in stdlib/src, which the host toolchain does not compile.
	OtherTree int
}

// GCDecision is one placement decision cmd/compile's -m printed.
type GCDecision struct {
	// File, Line and Col are the position -m printed, with the leading "./"
	// removed.
	File string
	Line int
	Col  int
	// Subject is the expression -m named, e.g. "&record{...}" or "new(record)"
	// or `"a string literal"`. It is what Kind is derived from and is worth
	// keeping for triage: it is the only description of the allocation gc gives.
	Subject string
	// Placement is what gc decided.
	Placement Placement
	// Kind classifies Subject.
	Kind Kind
	// Inlined records that this decision was printed at a position that also
	// carries an "inlining call to" diagnostic, which means it describes a copy
	// of an allocation written elsewhere. See the package comment.
	Inlined bool
	// Synthesized records that no -m line said this and the decision comes from
	// a documented gc rule instead. See channel handling in Join.
	Synthesized bool
	// Flow is the explanation -m=2 printed for this decision, when a level-2
	// build was supplied and its explanation joined onto this decision. See
	// ParseGCExplanations.
	Flow *GCFlow
	// Reason is Flow's category, empty when there is no explanation. Frame
	// placements never have one: gc explains an escape and says nothing about a
	// decision not to.
	Reason Reason
}

// GCReport is everything ParseGCFlagM got out of one program's -m output.
type GCReport struct {
	// Program is the corpus program's base name, e.g. "hello.go".
	Program string
	// Decisions are the placement decisions at positions inside Program.
	Decisions []GCDecision
	// InlineCalls is how many "inlining call to" diagnostics Program's own
	// positions carried.
	InlineCalls int
	// Foreign is diagnostics printed at a position outside Program: generic
	// instantiations and compiler-written wrappers, which -m attributes to
	// <autogenerated> or to a file in the host GOROOT. They describe code the
	// corpus program did not write and the census does not record under a
	// corpus position, so they are counted and dropped.
	Foreign int
	// Ignored counts diagnostics recognised as saying nothing about placement,
	// keyed by the phrase that identified them.
	Ignored map[string]int
	// Unknown is every diagnostic the parser did not recognise at all. It must
	// be empty: -m's wording is not stable across releases, and a parser that
	// silently skipped a message it did not know would under-count one side of
	// the matrix and look like agreement.
	Unknown []string
}

// gcPosition matches the "file:line:col: message" prefix -m puts on every
// diagnostic. The file may be "./prog.go" or an absolute GOROOT path. The
// column is optional: a diagnostic about compiler-written code carries the
// position "<autogenerated>:1:", with no column at all.
var gcPosition = regexp.MustCompile(`^(\S.*?):(\d+):(?:(\d+):)? (.*)$`)

// gcNonPlacement are the -m diagnostics that carry no placement decision. They
// are listed rather than skipped by a default arm so that a message a future
// release adds shows up in GCReport.Unknown instead of being silently ignored.
var gcNonPlacement = []string{
	"can inline ",
	"cannot inline ",
	"inline call to ",
	"leaking param",
	"parameter ",
	"devirtualizing ",
	"zero-copy string->[]byte conversion",
	"index bounds check elided",
	"marking ",
	"skip inlining ",
	"stack object ",
	"loop variable ",
}

// gcNonPlacementContains are non-placement diagnostics identified by a phrase
// in the middle of the message rather than a prefix, because the message starts
// with a function name.
var gcNonPlacementContains = []string{
	" ignoring self-assignment in ",
}

// ParseGCFlagM reads the stderr of `go build -gcflags=-m program` and returns
// the placement decisions it printed for positions inside program.
//
// It never skips a diagnostic it does not recognise. -m's wording changes
// between releases and this harness pins one; a message this parser has not
// been taught lands in GCReport.Unknown, and the caller is expected to refuse
// to publish a matrix that has any.
func ParseGCFlagM(program, output string) (GCReport, error) {
	report := GCReport{Program: program, Ignored: make(map[string]int)}
	inlineCallSites := make(map[[2]int]bool)
	type pending struct {
		line, col int
		subject   string
		placement Placement
	}
	var decisions []pending

	for _, text := range strings.Split(output, "\n") {
		if text == "" || strings.HasPrefix(text, "#") || strings.HasPrefix(text, "\t") {
			// "# command-line-arguments" is the package banner, and a leading
			// tab is a continuation line of a build error, not a diagnostic.
			continue
		}
		match := gcPosition.FindStringSubmatch(text)
		if match == nil {
			report.Unknown = append(report.Unknown, text)
			continue
		}
		file, message := match[1], match[4]
		if strings.TrimPrefix(file, "./") != program {
			report.Foreign++
			continue
		}
		line, _ := strconv.Atoi(match[2])
		col, _ := strconv.Atoi(match[3])

		if strings.HasPrefix(message, "inlining call to ") {
			report.InlineCalls++
			inlineCallSites[[2]int{line, col}] = true
			continue
		}
		if phrase, ignorable := ignorableGCMessage(message); ignorable {
			report.Ignored[phrase]++
			continue
		}
		switch {
		case strings.HasPrefix(message, "moved to heap: "):
			decisions = append(decisions, pending{line, col, strings.TrimPrefix(message, "moved to heap: "), Heap})
		case strings.HasSuffix(message, " escapes to heap"):
			decisions = append(decisions, pending{line, col, strings.TrimSuffix(message, " escapes to heap"), Heap})
		case strings.HasSuffix(message, " does not escape"):
			decisions = append(decisions, pending{line, col, strings.TrimSuffix(message, " does not escape"), Frame})
		default:
			report.Unknown = append(report.Unknown, text)
		}
	}

	for _, decision := range decisions {
		report.Decisions = append(report.Decisions, GCDecision{
			File:      program,
			Line:      decision.line,
			Col:       decision.col,
			Subject:   decision.subject,
			Placement: decision.placement,
			Kind:      subjectKind(decision.subject),
			Inlined:   inlineCallSites[[2]int{decision.line, decision.col}],
		})
	}
	return report, nil
}

func ignorableGCMessage(message string) (string, bool) {
	for _, prefix := range gcNonPlacement {
		if strings.HasPrefix(message, prefix) {
			return prefix, true
		}
	}
	for _, phrase := range gcNonPlacementContains {
		if strings.Contains(message, phrase) {
			return phrase, true
		}
	}
	return "", false
}

// mapLiteral matches a composite literal whose type is a map type, which -m
// prints as the type followed by "{...}".
var mapLiteral = regexp.MustCompile(`^map\[`)

// subjectKind classifies the expression -m named.
//
// The classification is by construct, not by the runtime call the construct
// lowers to, because gc's -m never names a runtime call. A subject it does not
// recognise is an object: that is where new(T), &T{...}, boxed values, closures
// and address-taken locals all land, and it is by far the common case.
func subjectKind(subject string) Kind {
	switch {
	case strings.HasPrefix(subject, "make(chan ") || strings.HasPrefix(subject, "make(<-chan ") || strings.HasPrefix(subject, "make(chan<- "):
		return KindChan
	case strings.HasPrefix(subject, "make(map["), mapLiteral.MatchString(subject):
		return KindMap
	case strings.HasPrefix(subject, "make([]"), strings.HasPrefix(subject, "[]"), strings.HasPrefix(subject, "..."):
		// "... argument" is the backing array -m names for a variadic call's
		// implicit slice.
		return KindSlice
	default:
		return KindObject
	}
}
