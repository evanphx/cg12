package goc_test

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/gcdiff"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The escape diagnostic's tests. What they pin is the *explanation*, not the
// placement: which placement each site gets is already pinned by the allocation
// census baseline over the whole corpus, and pinning it a second time here would
// only mean two files to update for one change.
//
// What is not covered anywhere else is that the reason printed beside a
// placement is the reason the compiler had. So each case below is a shape whose
// escape has one cause, and the test asserts that the cause is named -- and, at
// level 2, that the chain reaches the use that caused it.
//
// The three shapes are the ones a reader of this diagnostic will meet first:
// something that stays in the frame, something a call takes away, and something
// an interface conversion boxes.

// diagnoseEscapes compiles source with the diagnostic at the given level and
// returns the report, exactly as -m would print it.
//
// It sets the level around the compile rather than for the process, because the
// level is what the compiler reads to decide whether to record anything at all:
// a test that left it on would leave every later test in the package paying for
// explanations it does not read.
func diagnoseEscapes(t *testing.T, name, source string, level int) string {
	t.Helper()
	previous := opt.EscapeDiagLevel()
	opt.SetEscapeDiagLevel(level)
	defer opt.SetEscapeDiagLevel(previous)

	module, err := goc.CompileExecutable(name, []byte(source))
	require.NoError(t, err)

	var report bytes.Buffer
	opt.WriteEscapeDiagnostics(&report, module, name, level)
	return report.String()
}

// siteReport returns the block of the report for the site at the given
// position -- the decision line and every continuation line under it.
//
// More than one block can carry the same position: a function inlined into its
// caller is decided again in the caller, and each copy is reported. They are
// joined, so an assertion about "the site on this line" is an assertion about
// every copy of it.
func siteReport(report, position string) string {
	var blocks []string
	var current []string
	inSite := false
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, "\t") {
			if inSite {
				current = append(current, line)
			}
			continue
		}
		if inSite {
			blocks = append(blocks, strings.Join(current, "\n"))
			current = nil
		}
		inSite = strings.HasPrefix(line, position+":")
		if inSite {
			current = []string{line}
		}
	}
	if inSite {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	return strings.Join(blocks, "\n")
}

// escapeDiagnosticProgram carries one allocation of each shape, and nothing
// else, so that every line of the report has a reason to be there.
//
//   - framed's literal is read and dropped, so it stays where it is made.
//   - throughCall's is handed to a function that stores it in a package-level
//     variable, which is the whole of why it cannot stay.
//   - throughInterface's slice is copied into an any, and boxing copies the
//     header into fresh storage that the backing array is then reachable from.
//
// keepPointer and keepAny are deliberately in this file rather than reduced to
// `sink = ...` at the call site: the point of the chain at level 2 is that it
// crosses into the callee, and there is nothing to cross if the publication is
// written where the object is.
const escapeDiagnosticProgram = `package main

type point struct{ x, y int }

var keptPointer *point
var keptAny any

func keepPointer(p *point) { keptPointer = p }

func keepAny(value any) { keptAny = value }

func framed() int {
	p := &point{x: 1, y: 2}
	return p.x + p.y
}

func throughCall() {
	p := &point{x: 3, y: 4}
	keepPointer(p)
}

func throughInterface() {
	values := []int{7, 8, 9}
	keepAny(values)
}

func main() {
	println(framed())
	throughCall()
	throughInterface()
	println(keptPointer.x)
}
`

// TestEscapeDiagnosticNamesWhereEachAllocationWent is the level-1 statement: a
// decision line per site, worded as gc words it, with the rule that decided
// under it.
func TestEscapeDiagnosticNamesWhereEachAllocationWent(t *testing.T) {
	report := diagnoseEscapes(t, "escape_diagnostic.go", escapeDiagnosticProgram, 1)

	framed := siteReport(report, "escape_diagnostic.go:13:7")
	require.NotEmpty(t, framed, "no report for the literal in framed:\n%s", report)
	assert.Contains(t, framed, "main_point does not escape")
	assert.NotContains(t, framed, "rule:",
		"a frame placement is the absence of a publication and has no rule to name")

	viaCall := siteReport(report, "escape_diagnostic.go:18:7")
	require.NotEmpty(t, viaCall, "no report for the literal in throughCall:\n%s", report)
	assert.Contains(t, viaCall, "main_point escapes to heap")
	assert.Contains(t, viaCall, "rule: assigned to the package-level variable keptPointer")

	boxed := siteReport(report, "escape_diagnostic.go:23:12")
	require.NotEmpty(t, boxed, "no report for the slice in throughInterface:\n%s", report)
	assert.Contains(t, boxed, "escapes to heap")
	assert.Contains(t, boxed, "rule: converted to any, and boxing a []int makes fresh storage for the payload")
}

// TestEscapeDiagnosticLevelTwoNamesTheChain is the reason the diagnostic exists.
//
// "it escapes" is what every instrument built for these defects so far already
// said. The chain is what none of them said: which question the walk was
// answering when it gave up, and where the use it gave up on is written. For
// throughCall that is three links -- the literal, the argument position, the
// callee's own parameter -- ending at the assignment inside keepPointer.
func TestEscapeDiagnosticLevelTwoNamesTheChain(t *testing.T) {
	report := diagnoseEscapes(t, "escape_diagnostic.go", escapeDiagnosticProgram, 2)

	viaCall := siteReport(report, "escape_diagnostic.go:18:7")
	require.NotEmpty(t, viaCall, "no report for the literal in throughCall:\n%s", report)
	assert.Contains(t, viaCall, "from: argument 0 of the call to main.keepPointer")
	assert.Contains(t, viaCall, "from: p, declared at escape_diagnostic.go:18:2")
	assert.Contains(t, viaCall, "at:   escape_diagnostic.go:8:30",
		"the chain must end at the assignment inside keepPointer")

	// The links come out from the deciding use outwards, which is the order the
	// path reads in: the callee's parameter, then the call, then the caller's
	// own object.
	callee := strings.Index(viaCall, "from: p, declared at escape_diagnostic.go:8:18")
	call := strings.Index(viaCall, "from: argument 0 of the call to")
	caller := strings.Index(viaCall, "from: p, declared at escape_diagnostic.go:18:2")
	require.Positive(t, callee)
	assert.Less(t, callee, call)
	assert.Less(t, call, caller)
}

// TestEscapeDiagnosticLevelOneCarriesNoChain pins that the level is a level: a
// reader who asked for placements and rules does not get paths.
func TestEscapeDiagnosticLevelOneCarriesNoChain(t *testing.T) {
	report := diagnoseEscapes(t, "escape_diagnostic.go", escapeDiagnosticProgram, 1)
	assert.NotContains(t, report, "from: ")
}

// TestEscapeDiagnosticOffRecordsNothing is the zero-cost claim, stated where it
// can be checked: with the level at 0 no reason, use or chain is recorded on any
// placement record in the module, so nothing was formatted and nothing was
// allocated to hold it.
//
// The byte-identical-code half of the claim is
// TestEscapeDiagnosticDoesNotChangeTheEmittedModule is the guard the flag has to
// carry: turning the diagnostic on must not move a byte of code.
//
// The comparison is the serialized module, as in
// TestCompilingTheSameSourceTwiceGivesTheSameModule, and it is exact rather than
// filtered: ir.Module's binary encoding does not carry PlacedAllocs or
// AllocDecisions at all, so everything the diagnostic writes is outside it by
// construction and a single byte of difference is a real one.
func TestEscapeDiagnosticDoesNotChangeTheEmittedModule(t *testing.T) {
	previous := opt.EscapeDiagLevel()
	defer opt.SetEscapeDiagLevel(previous)

	opt.SetEscapeDiagLevel(0)
	off, err := goc.CompileExecutable("escape_diagnostic.go", []byte(escapeDiagnosticProgram))
	require.NoError(t, err)

	opt.SetEscapeDiagLevel(2)
	on, err := goc.CompileExecutable("escape_diagnostic.go", []byte(escapeDiagnosticProgram))
	require.NoError(t, err)

	require.Equal(t, len(off.Funcs), len(on.Funcs))
	for index := range off.Funcs {
		require.Equal(t, off.Funcs[index].String(), on.Funcs[index].String(),
			"%s was compiled differently with the diagnostic on", off.Funcs[index].Name)
	}

	offBytes, err := off.MarshalBinary()
	require.NoError(t, err)
	onBytes, err := on.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(offBytes), sha256.Sum256(onBytes),
		"the escape diagnostic changed the emitted module")
}

// TestEscapeDiagnosticParsesAsGCFlagM is the interoperability claim: goc's -m
// output goes through internal/gcdiff's parser for cmd/compile's -m without a
// single unrecognised line.
//
// That parser is deliberately strict -- it refuses to skip a diagnostic it has
// not been taught, so that a wording change in a future Go release shows up as
// an Unknown rather than as a silently missing decision. Passing it is therefore
// a real statement about the output's shape and not just about its prefix.
//
// What it buys, and what it does not, is written up in
// docs/escape-diagnostics.md.
func TestEscapeDiagnosticParsesAsGCFlagM(t *testing.T) {
	report := diagnoseEscapes(t, "escape_diagnostic.go", escapeDiagnosticProgram, 2)

	parsed, err := gcdiff.ParseGCFlagM("escape_diagnostic.go", report)
	require.NoError(t, err)
	require.Empty(t, parsed.Unknown,
		"the -m parser did not recognise every line of goc's own -m output")
	require.NotEmpty(t, parsed.Decisions)

	frames, heaps := 0, 0
	for _, decision := range parsed.Decisions {
		switch decision.Placement {
		case gcdiff.Frame:
			frames++
		case gcdiff.Heap:
			heaps++
		}
	}
	assert.Positive(t, frames, "no frame placement survived the round trip")
	assert.Positive(t, heaps, "no heap placement survived the round trip")

	// The continuation lines are what carries goc's reasons, and the parser
	// drops them exactly as it drops gc's own continuations. That is the price
	// of the shape: a reason-aware join needs ParseGCFlagM to keep them.
	assert.Zero(t, parsed.Foreign)
	assert.Zero(t, parsed.InlineCalls)
}
