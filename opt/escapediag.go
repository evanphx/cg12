package opt

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"sync/atomic"

	"github.com/evanphx/cg12/ir"
)

// The escape diagnostic: goc's answer to `go build -gcflags=-m`.
//
// # What it is for
//
// Every escape-analysis defect found in this tree so far was found by building a
// new instrument -- a static audit of the emitted IR, an allocation census with a
// two-way baseline, a differential against cmd/compile's own -m, a slog
// allocation benchmark. Each of those answers "where did this object land"
// eventually, and none of them answers "why". This does, and it answers it in the
// compiler, at the moment the decision is made.
//
// # Where the answers come from
//
// Nowhere new. goc places an allocation in one of two places, and both already
// record what they did at the moment they did it:
//
//   - the AST walk in package goc, which commits most allocations itself and
//     records ir.PlacedAlloc;
//   - opt.LowerHeapAllocations, which decides the neutral OHeapAlloc candidates
//     and records ir.AllocDecision.
//
// This adds a reason to each of those records, written by the code that decides,
// and prints them. It does not re-run either analysis and it has no rules of its
// own. That is deliberate: a diagnostic that recomputes the answer can disagree
// with what the compiler did, and two components enforcing one rule differently
// is a defect this tree has shipped twice.
//
// # Cost when off
//
// EscapeDiagLevel is 0 unless -m or GOC_M asks otherwise, and at 0:
//
//   - the IR pass's per-allocation reason map is never allocated and no reason
//     string is ever formatted (analyzeCandidateEscapes's wantReasons);
//   - the AST walk's explanation state is a nil pointer, and every hook on it is
//     a nil check;
//   - nothing is printed.
//
// The records themselves -- ir.AllocDecision and ir.PlacedAlloc -- were already
// being made on every compile before this existed. TestEscapeDiagnosticsAreFree
// pins the byte-identical-output claim.
//
// # Output shape
//
// One decision line per site, in cmd/compile's -m wording:
//
//	prog.go:12:9: main.point does not escape
//	prog.go:20:16: main.node escapes to heap
//
// followed by tab-indented detail lines, which is how gc formats continuations
// and which internal/gcdiff's -m parser already skips. Level 1 gives the rule
// that decided; level 2 adds the chain of questions between the object and the
// use that answered, in the way -m=2 explains an escape path.

// escapeDiagLevel is the -m level. It is read from the environment once, and
// -m on the command line overrides it.
var escapeDiagLevel atomic.Int32

func init() {
	escapeDiagLevel.Store(int32(parseEscapeDiagLevel(os.Getenv("GOC_M"))))
}

// parseEscapeDiagLevel reads the environment form of -m. An unparseable value is
// level 1, matching escapeDebugLevel's reading of GOC_DEBUG_ESCAPE: a knob set
// to something that is not a number was meant to be on.
func parseEscapeDiagLevel(setting string) int {
	if setting == "" {
		return 0
	}
	level, err := strconv.Atoi(setting)
	if err != nil {
		return 1
	}
	if level < 0 {
		return 0
	}
	return level
}

// EscapeDiagLevel reports the escape diagnostic level: 0 off, 1 placements with
// the rule that decided them, 2 also the chain that led to the deciding use.
func EscapeDiagLevel() int { return int(escapeDiagLevel.Load()) }

// SetEscapeDiagLevel sets the level. cmd/goc's -m calls it; so do the tests.
func SetEscapeDiagLevel(level int) {
	if level < 0 {
		level = 0
	}
	escapeDiagLevel.Store(int32(level))
}

// EscapeSite is one allocation site as the diagnostic reports it: where it is,
// where the object went, and which of the two placers decided.
type EscapeSite struct {
	// File, Line and Col are the source position. A site the front end gave no
	// position is reported with an empty File, since a diagnostic that cannot be
	// pointed at is still worth counting.
	File string
	Line int
	Col  int
	// Func is the IR function containing the allocation, after inlining.
	Func string
	// Decider names which analysis placed it: "front end" for goc's AST walk,
	// "ir pass" for opt.LowerHeapAllocations.
	Decider string
	// Site is the construct the front end recorded, or the allocator the IR pass
	// candidate named. Together with Type it is the subject printed on the
	// decision line.
	Site      string
	Allocator string
	Type      string
	Placement ir.AllocPlacement
	// Rule is the rule that decided, in the deciding analysis's own words. It is
	// empty for a frame placement, which no single rule explains: the walk
	// simply found nothing that publishes the object.
	Rule string
	// Use is the position of the publication the walk found, when it found one.
	Use string
	// Chain is the sequence of questions between the object and Use, from the use
	// outwards. Level 2 only.
	Chain []string
}

// Subject is what the decision line names, in the slot -m puts the source
// expression in. goc has no rendering of the source expression at the point it
// decides, so it names the type descriptor the object would be allocated
// under -- which is the identity the allocation census uses too.
func (site EscapeSite) Subject() string {
	if site.Type != "" {
		// Rendered the way the allocation census renders it, so that a line of
		// this report and a line of goc/testdata/alloc_census_baseline.txt name
		// the same site with the same string.
		return AllocationTypeName(site.Type)
	}
	if site.Allocator != "" {
		return site.Allocator
	}
	if site.Site != "" {
		return site.Site
	}
	return "?"
}

func (site EscapeSite) position() string {
	if site.File == "" {
		return "?"
	}
	return fmt.Sprintf("%s:%d:%d", site.File, site.Line, site.Col)
}

// verdict is the decision line's wording, taken from -m so that a reader who
// knows gc's output knows this one.
func (site EscapeSite) verdict() string {
	if site.Placement == ir.AllocInFrame {
		return "does not escape"
	}
	return "escapes to heap"
}

func (site EscapeSite) sortKey() string {
	return fmt.Sprintf("%s|%09d|%09d|%s|%s|%s|%s",
		site.File, site.Line, site.Col, site.Func, site.Decider, site.Site, site.Subject())
}

// EscapeSites collects every placement decision in module, from both placers.
//
// program, when non-empty, restricts the report to that source file -- the file
// the compiler was pointed at. That is what -m does: it reports the package
// being compiled and not the standard library it imported, and goc compiles a
// whole program including a vendored standard library, so without the
// restriction the useful lines are lost among ten thousand others.
func EscapeSites(module *ir.Module, program string) []EscapeSite {
	var sites []EscapeSite
	keep := func(file string) bool {
		return program == "" || file == program
	}

	for _, function := range module.Funcs {
		if len(function.PlacedAllocs) == 0 {
			continue
		}
		positions := allocationDefinitions(function, function.PlacedAllocs)
		for id, placed := range function.PlacedAllocs {
			position := positions[id]
			file := module.FileName(position.File)
			if !keep(file) {
				continue
			}
			sites = append(sites, EscapeSite{
				File:      file,
				Line:      int(position.Line),
				Col:       int(position.Col),
				Func:      function.Name,
				Decider:   "front end",
				Site:      placed.Site,
				Allocator: placed.Allocator,
				Type:      placed.Type,
				Placement: placed.Placement,
				Rule:      placed.Rule,
				Use:       positionString(module, placed.Use),
				Chain:     placed.Chain,
			})
		}
	}

	for _, decision := range module.AllocDecisions {
		file := module.FileName(decision.Pos.File)
		if !keep(file) {
			continue
		}
		sites = append(sites, EscapeSite{
			File:      file,
			Line:      int(decision.Pos.Line),
			Col:       int(decision.Pos.Col),
			Func:      decision.Func,
			Decider:   "ir pass",
			Site:      "heap-alloc-candidate",
			Allocator: decision.Allocator,
			Type:      decision.Type,
			Placement: decision.Placement,
			Rule:      decision.Reason,
			Use:       positionString(module, decision.Use),
		})
	}

	sort.Slice(sites, func(i, j int) bool { return sites[i].sortKey() < sites[j].sortKey() })
	return sites
}

func positionString(module *ir.Module, position ir.SrcPos) string {
	if !position.Valid() {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d", module.FileName(position.File), position.Line, position.Col)
}

// WriteEscapeDiagnostics prints the report for module at the given level.
//
// The decision line is shaped exactly like cmd/compile's, and everything goc has
// to say beyond it is on tab-indented continuation lines. gc uses a leading tab
// for its own continuations and internal/gcdiff's parser skips them, so this
// output can be fed to the same parser that reads gc's.
func WriteEscapeDiagnostics(w io.Writer, module *ir.Module, program string, level int) {
	if level < 1 {
		return
	}
	for _, site := range EscapeSites(module, program) {
		fmt.Fprintf(w, "%s: %s %s\n", site.position(), site.Subject(), site.verdict())
		fmt.Fprintf(w, "\t%s: %s in %s\n", site.Decider, site.Site, site.Func)
		if site.Rule != "" {
			fmt.Fprintf(w, "\trule: %s\n", site.Rule)
		}
		if level < 2 {
			continue
		}
		for _, step := range site.Chain {
			fmt.Fprintf(w, "\tfrom: %s\n", step)
		}
		if site.Use != "" {
			fmt.Fprintf(w, "\tat:   %s\n", site.Use)
		}
	}
}
