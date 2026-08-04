package goc_test

import (
	"bytes"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/gcdiff"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// measureGCReasonDifferential asks for the reason differential to be run. It is
// opt-in for the same reasons TestEscapeDifferentialAgainstGC is, and one more:
// it compiles the whole corpus with goc as well as twice with the host
// toolchain, so it is the most expensive thing in this package.
var measureGCReasonDifferential = flag.Bool("escape-gc-reason-differential", false,
	"compare the reason each compiler gives for each allocation's placement, not only the placement")

// updateGCReasonDifferential rewrites the checked-in output from this run.
var updateGCReasonDifferential = flag.Bool("update-escape-gc-reason-differential", false,
	"rewrite testdata/escape_gc_reason_differential.txt from this run")

const gcReasonDifferentialPath = "testdata/escape_gc_reason_differential.txt"

const gcReasonDifferentialHeader = `# What each compiler says is the REASON an object went where it went, joined per
# source line, over the whole corpus. The companion of
# escape_gc_differential.txt, which compares only the placements.
#
# Regenerate with
#
#     go test ./goc -run TestEscapeReasonDifferentialAgainstGC -timeout 60m \
#         -escape-gc-reason-differential -update-escape-gc-reason-differential
#
# and read the diff. That one command is the whole procedure; it needs a Go
# toolchain on PATH and takes about ten minutes. Re-run it after any change to
# where goc puts an allocation -- including a merge that brings one in -- because
# the placement half of this file comes from the committed allocation census and
# the reason half is compiled fresh, and the coverage table's "lines where goc -m
# and the census disagree" is what tells you the two have drifted apart.
#
# Two things are being compared that are not the same kind of thing, and the
# join is documented at length in package internal/gcdiff:
#
#   - placement, per source line, exactly as escape_gc_differential.txt computes
#     it: goc's side from the committed alloc_census_baseline.txt, gc's from
#     "go build -gcflags=-m".
#   - reason, from goc's own "-m" report and from a second host build at
#     "-gcflags=-m=2", normalised into the categories the next section lists.
#
# The reasons are normalised because the two vocabularies are not translations
# of each other and gc's is internal notation that moves between releases. The
# host toolchain is recorded below for that reason; a diff against a run on a
# different release is not a change in goc.
#
# Every position here is relative to the repository root, including the "at"
# lines, which name the use that decided and can be inside the vendored standard
# library. goc interns those absolutely -- it finds stdlib/ through
# runtime.Caller -- so rendering them as the compiler produced them wrote the
# generating machine's checkout into 42 lines of this file, and the same commit
# in another directory then failed the comparison on paths alone. This file is
# meant to be regenerable by anyone, anywhere; if a diff of it shows a path,
# that has stopped being true.
#
`

// TestEscapeReasonDifferentialAgainstGC compares why, not just where.
//
// # What this can see that the placement differential cannot
//
// Two compilers can agree that an object belongs on the heap and disagree
// completely about what put it there. Where that happens one of them is
// reaching the right answer by a route the other does not recognise, and a
// placement comparison is blind to it by construction: both cells say "heap"
// and the line never appears in either direction of escape_gc_differential.txt.
// Those lines are this file's headline section.
//
// The reverse is worth as much and is shaped differently. Neither compiler
// explains a decision to keep an object in a frame -- gc prints "does not
// escape" and stops, and goc deliberately does the same -- so on a line the two
// place differently only one of them has anything to say. What it says is the
// triage: for the permissive direction, gc's category states directly what it
// thinks publishes an object goc kept in a frame.
//
// # Why it asserts almost nothing, again
//
// The same reason the placement differential does not: there is no number of
// disagreements that is correct, and the file exists so that a person looks at
// what moved. What it does assert is that the instrument is whole -- every
// reason on both sides fell into a category, every line of goc's own -m parsed,
// and goc's -m agrees with the committed census about where things went. Each
// of those failing means the comparison quietly stopped covering something.
func TestEscapeReasonDifferentialAgainstGC(t *testing.T) {
	if !*measureGCReasonDifferential && !*updateGCReasonDifferential {
		t.Skip("pass -escape-gc-reason-differential to compare the reasons as well as the placements")
	}

	goVersion := hostGoVersion(t)
	t.Logf("host toolchain: %s", goVersion)

	paths, err := filepath.Glob(filepath.Join(corpusDir, "*.go"))
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	sort.Strings(paths)

	censusText, err := os.ReadFile(allocCensusPath)
	require.NoError(t, err)
	census, drops, err := gcdiff.ParseCensus(string(censusText), corpusDir)
	require.NoError(t, err)

	programs := buildCorpusWithHostToolchain(t, paths)
	addGCExplanations(t, paths, programs)
	addGocReports(t, paths, programs)

	result := gcdiff.Join(census, drops, programs, gcdiff.Options{})
	gcdiff.JoinReasons(&result, programs)

	t.Logf("reasons on both sides for %d programs; %d goc rules, %d gc explanations joined",
		result.ReasonCoverage.Programs, result.ReasonCoverage.GocRules, result.ReasonCoverage.GCExplainedJoined)
	t.Logf("agree on placement, disagree on reason: %d lines", len(result.ReasonDisagreements()))
	t.Logf("disagree on placement, agree on reason: %d lines", len(result.ReasonAgreementsAcrossPlacements()))

	// filepath.Dir of the vendored standard library is this checkout's root, and
	// it is the root the `at:` positions are absolute against: the paths in them
	// were interned by the same goc.StdlibRoot, in this same binary. Passing it
	// is what keeps the rendered file the same bytes in every directory -- see
	// gcdiff.relativeToRepository, and TestReasonPositionsAreRepositoryRelative
	// below for the assertion that no absolute path survives.
	rendered := gcdiff.RenderReasons(gcReasonDifferentialHeader, goVersion,
		filepath.Dir(goc.StdlibRoot()), result)
	if *updateGCReasonDifferential {
		require.NoError(t, os.WriteFile(gcReasonDifferentialPath, []byte(rendered), 0o644))
		t.Skip("output rewritten; rerun without -update-escape-gc-reason-differential to check it")
	}

	assert.Empty(t, result.ReasonCoverage.UnknownGocLines,
		"goc's own -m printed a line internal/gcdiff could not read. Unlike gc's -m this is\n"+
			"this tree's own output, so it is a change to the diagnostic the differential has\n"+
			"not been taught. Teach internal/gcdiff.ParseGocFlagM about it.")
	assert.Empty(t, result.ReasonCoverage.UncategorisedGocRules,
		"a goc rule fell outside the taxonomy. A reason that does not categorise removes a\n"+
			"line from the comparison and makes the two compilers look as though they agreed\n"+
			"about it. Add it to internal/gcdiff's gocRulePrefixes.")
	assert.Empty(t, result.ReasonCoverage.UncategorisedGCFlows,
		"a cmd/compile flow edge fell outside the taxonomy. Expected the first time this runs\n"+
			"against a release that adds one; add it to internal/gcdiff's gcFlowEdges.")
	assert.Zero(t, result.ReasonCoverage.GocLinesContradicting,
		"goc's -m and the committed allocation census contradict each other about where an\n"+
			"allocation on some line went. The census is committed and the -m report is\n"+
			"compiled fresh, so this means the tree moved under the census: rerun\n"+
			"TestAllocationCensus with -update-alloc-census-baseline, read that diff, and then\n"+
			"rerun this. Lines only one of the two records are a scope difference, not drift,\n"+
			"and are reported in the file rather than asserted on.\n  %s",
		strings.Join(result.ReasonCoverage.GocLinesContradictingList, "\n  "))

	accepted, err := os.ReadFile(gcReasonDifferentialPath)
	require.NoError(t, err)
	assert.Equal(t, string(accepted), rendered,
		"the reason differential moved. If the host toolchain is not the one recorded in the\n"+
			"file, that is the whole explanation and the fix is to rerun with\n"+
			"-update-escape-gc-reason-differential. Otherwise either goc's explanation for a\n"+
			"placement changed or the placement did, and the diff says which lines.")
}

// TestReasonPositionsAreRepositoryRelative is the cheap half of the guarantee
// that TestEscapeReasonDifferentialAgainstGC reproduces outside the directory it
// was generated in.
//
// The expensive half is regenerating the file in a second checkout at a
// different path and diffing the two, which is what proves it and which takes
// ten minutes twice. This reads the committed bytes instead and takes no time at
// all, so it runs in the ordinary suite: an absolute position in this file means
// the generator has started emitting the generating machine's paths again, and
// the next person to run the differential anywhere else fails on path noise
// rather than on a compiler change. It was true of 42 lines before
// gcdiff.relativeToRepository existed.
func TestReasonPositionsAreRepositoryRelative(t *testing.T) {
	text, err := os.ReadFile(gcReasonDifferentialPath)
	require.NoError(t, err)

	// A position, not merely a path: `//go:noescape` appears in this file as
	// part of a rule and begins with a slash too.
	absolutePosition := regexp.MustCompile(`(?m)^.*[\s]/\S*:\d+:\d+.*$`)
	assert.Empty(t, absolutePosition.FindAllString(string(text), -1),
		"%s names an absolute source position, so it records the directory it was generated\n"+
			"in and nobody else can reproduce it. Every position in it should be relative to\n"+
			"the repository root; see gcdiff.relativeToRepository.", gcReasonDifferentialPath)
}

// addGCExplanations fills in each program's -m=2 explanations, from a second
// host build.
//
// A second build rather than reading the placements out of -m=2 as well: -m=2's
// output is a superset of -m's, so one build could serve both, but the placement
// half of this comparison is the committed escape_gc_differential.txt's, and
// putting a baseline several jobs have already accepted at the mercy of a
// parser change is a bad trade for two minutes. ParseGCFlagM is not touched.
//
// A program the host toolchain could not build has no Report, and gets no
// explanations either; it is already counted as not compared.
func addGCExplanations(t *testing.T, paths []string, programs []gcdiff.Program) {
	t.Helper()

	scratch := stageCorpus(t, paths)
	forEachProgram(programs, func(program *gcdiff.Program) {
		if program.Report == nil {
			return
		}
		command := exec.Command("go", "build", "-o", os.DevNull, "-gcflags=-m=2", program.Name)
		command.Dir = scratch
		command.Env = append(os.Environ(), "GOFLAGS=")
		output, err := command.CombinedOutput()
		if err != nil {
			// The level-1 build of this program succeeded, so a failure here is
			// not the program's fault and must not be swallowed.
			t.Errorf("%s built at -m but not at -m=2: %v\n%s", program.Name, err, output)
			return
		}
		explanations, parseError := gcdiff.ParseGCExplanations(program.Name, string(output))
		if parseError != nil {
			t.Errorf("%s: parsing -m=2: %v", program.Name, parseError)
			return
		}
		program.Explanations = &explanations
	})
}

// addGocReports compiles each corpus program with goc, with the escape
// diagnostic on, and parses the report it prints.
//
// The report is taken per module rather than off the compiler's own stream: the
// diagnostic level is a process-wide setting, so with the corpus compiling
// concurrently the compiler's own copies would arrive interleaved. The
// compiler's copy is sent to io.Discard and opt.WriteEscapeDiagnostics is called
// here instead, which writes the same bytes.
//
// Level 2 rather than 1 because level 2 adds the position of the use that
// decided, and that position is the thing gc's flow chain also names -- it is
// what lets a reader check the two explanations against each other instead of
// only counting them.
func addGocReports(t *testing.T, paths []string, programs []gcdiff.Program) {
	t.Helper()

	const level = 2
	previousLevel := opt.EscapeDiagLevel()
	opt.SetEscapeDiagLevel(level)
	opt.SetEscapeDiagWriter(io.Discard)
	defer func() {
		opt.SetEscapeDiagLevel(previousLevel)
		opt.SetEscapeDiagWriter(nil)
	}()

	byName := make(map[string]string, len(paths))
	for _, path := range paths {
		byName[filepath.Base(path)] = path
	}

	forEachProgram(programs, func(program *gcdiff.Program) {
		path := byName[program.Name]
		source, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			return
		}
		module, err := goc.CompileExecutable(path, source)
		if err != nil {
			// Every corpus program compiles -- TestFrameEscapeAudit requires it
			// -- so one that does not is a failure and not a coverage note.
			t.Errorf("%s: goc could not compile it: %v", path, err)
			return
		}
		var report bytes.Buffer
		opt.WriteEscapeDiagnostics(&report, module, path, level)
		parsed, parseError := gcdiff.ParseGocFlagM(program.Name, report.String())
		if parseError != nil {
			t.Errorf("%s: parsing goc -m: %v", path, parseError)
			return
		}
		program.Goc = &parsed
	})
}

// stageCorpus writes every corpus program into one scratch module, the way
// buildCorpusWithHostToolchain does: `go build prog.go` compiles only the files
// named, so hundreds of package mains can share a directory.
func stageCorpus(t *testing.T, paths []string) string {
	t.Helper()

	scratch := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(scratch, "go.mod"),
		[]byte("module gcdiffcorpus\n\ngo 1.21\n"), 0o644))
	for _, path := range paths {
		source, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(scratch, filepath.Base(path)), source, 0o644))
	}
	return scratch
}

// forEachProgram runs work over every program concurrently, bounded the way
// compileCorpusForAudits bounds itself: goc's peak is several hundred megabytes
// and multiplying it by the core count on a 64-core machine is not a good
// trade.
func forEachProgram(programs []gcdiff.Program, work func(*gcdiff.Program)) {
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	queue := make(chan int)
	var working sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		working.Add(1)
		go func() {
			defer working.Done()
			for index := range queue {
				work(&programs[index])
			}
		}()
	}
	for index := range programs {
		queue <- index
	}
	close(queue)
	working.Wait()
}

// TestReasonTaxonomyCoversBothVocabularies pins the classifier against a sample
// of each compiler's real output, so the taxonomy stays covered without running
// the corpus.
//
// Every string below was taken from an actual sweep of the corpus. The point is
// not that these particular sentences are classified correctly -- it is that a
// change to either compiler's wording that silently drops a rule out of the
// taxonomy fails here in seconds instead of in a ten-minute run.
func TestReasonTaxonomyCoversBothVocabularies(t *testing.T) {
	for _, testCase := range []struct {
		rule   string
		reason gcdiff.Reason
	}{
		{"write barrier into a candidate", gcdiff.ReasonStoredInObject},
		{"store into non-local storage", gcdiff.ReasonStoredInObject},
		{"write barrier into non-local storage", gcdiff.ReasonStoredInObject},
		{"assigned to the package-level variable escapeSink", gcdiff.ReasonStoredInObject},
		{"argument 1 of $fmt.Sprintf may retain something it points at", gcdiff.ReasonCallRetains},
		{"argument 0 of $main.keepElement may retain something inside a self-referential object", gcdiff.ReasonCallRetains},
		{"argument 1 of $os.File.Read escapes", gcdiff.ReasonCallRetains},
		{"argument 0 of $main.f leaks to a result the caller cannot follow", gcdiff.ReasonCallRetains},
		{"call to $main.f", gcdiff.ReasonCallRetains},
		{"used as the receiver of main.T.M, which may retain it", gcdiff.ReasonCallRetains},
		{"passed to main.f, which may retain argument 0", gcdiff.ReasonCallRetains},
		{"passed in the variadic position of main.f, whose ... parameter the walk cannot prove does not hold its elements", gcdiff.ReasonCallRetains},
		// The three runtime lowerings, translated back to the construct.
		{"argument 0 of $runtime.newproc escapes", gcdiff.ReasonClosureCaptured},
		{"argument 0 of $runtime.deferproc escapes", gcdiff.ReasonClosureCaptured},
		{"argument 0 of $runtime.chansend1 escapes", gcdiff.ReasonChannelSend},
		// A generic instantiation of an ordinary callee is still a call.
		{"argument 1 of $runtime.AddCleanup[main.maskedBox,struct{}] escapes", gcdiff.ReasonCallRetains},
		{"passed to (net.Conn).Write, whose declaration this compilation does not have", gcdiff.ReasonCallOpaque},
		{"passed to runtime/metrics.runtime_readMetrics, which has no Go body and is not marked //go:noescape", gcdiff.ReasonCallOpaque},
		{"passed to main.descend, which the walk is already inside: it breaks the recursion by answering \"escapes\"", gcdiff.ReasonCallOpaque},
		{"passed to a call the walk cannot resolve to a single function", gcdiff.ReasonCallOpaque},
		{"call to $main.f, which is not a module function", gcdiff.ReasonCallOpaque},
		{"indirect call", gcdiff.ReasonCallOpaque},
		{"returned", gcdiff.ReasonReturned},
		{"converted to any, and boxing a []int makes fresh storage for the payload", gcdiff.ReasonInterfaceBoxed},
		{"allocated in a loop; one frame slot cannot hold one object per iteration", gcdiff.ReasonLoopCarried},
		{"its address is still reachable on the next iteration of the enclosing loop", gcdiff.ReasonLoopCarried},
		{"holds more separately-allocated payloads than it costs to hold them", gcdiff.ReasonFolded},
		{"phi", gcdiff.ReasonFolded},
		{"read back out of the object holding it", gcdiff.ReasonReadOut},
		{"block-copied out of the object holding it", gcdiff.ReasonReadOut},
		{"the walk found a use it could not prove local", gcdiff.ReasonUnexplained},
		{"node is used here in a way the walk cannot prove keeps it local", gcdiff.ReasonUnexplained},
		{"counter is captured by a function literal that escapes", gcdiff.ReasonClosureCaptured},
		// Both analyses word the size bound the same way, and both start it
		// with the object's own size, so neither has a fixed prefix.
		{"1600000 bytes is larger than the 65536 a frame will hold", gcdiff.ReasonTooLarge},
		{"524288 bytes is larger than the 65536 a frame will hold", gcdiff.ReasonTooLarge},
		{"", ""},
	} {
		reason, known := gcdiff.ClassifyGocRule(testCase.rule)
		assert.True(t, known, "goc rule not categorised: %q", testCase.rule)
		assert.Equal(t, testCase.reason, reason, "goc rule %q", testCase.rule)
	}

	for _, testCase := range []struct {
		name   string
		flow   gcdiff.GCFlow
		reason gcdiff.Reason
	}{
		{
			name:   "the deciding edge is the last one that is not an artefact",
			flow:   gcdiff.GCFlow{Dest: "{heap}", Source: "&{storage for x}", Edges: []string{"spill", "call parameter"}},
			reason: gcdiff.ReasonCallRetains,
		},
		{
			name:   "a dereference sink has no edges of its own, so the edge before it decided",
			flow:   gcdiff.GCFlow{Dest: "{heap}", Source: "*{temp}", Edges: []string{"spill", "assign-pair", "dot", "call parameter"}},
			reason: gcdiff.ReasonCallRetains,
		},
		{
			name:   "a func literal with nothing but a spill is a closure the compiler heaped",
			flow:   gcdiff.GCFlow{Dest: "{heap}", Source: "&{storage for func literal}", Edges: []string{"spill"}},
			reason: gcdiff.ReasonClosureCaptured,
		},
		{
			name:   "a chain ending at a result left through the result",
			flow:   gcdiff.GCFlow{Dest: "~r0", Source: "&x", Edges: []string{"address-of"}},
			reason: gcdiff.ReasonReturned,
		},
		{
			name:   "an inlined callee's result is still a result",
			flow:   gcdiff.GCFlow{Dest: "sync.~r0", Source: "&x", Edges: []string{"spill"}},
			reason: gcdiff.ReasonReturned,
		},
		{
			name:   "boxing",
			flow:   gcdiff.GCFlow{Dest: "{storage for &T{...}}", Source: "x", Edges: []string{"spill", "interface-converted"}},
			reason: gcdiff.ReasonInterfaceBoxed,
		},
		{
			name:   "a struct literal element is a store into another object",
			flow:   gcdiff.GCFlow{Dest: "{storage for &T{...}}", Source: "x", Edges: []string{"spill", "struct literal element"}},
			reason: gcdiff.ReasonStoredInObject,
		},
		{
			name:   "a channel send",
			flow:   gcdiff.GCFlow{Dest: "{heap}", Source: "&x", Edges: []string{"spill", "send"}},
			reason: gcdiff.ReasonChannelSend,
		},
		{
			name:   "too large for a frame",
			flow:   gcdiff.GCFlow{Dest: "{heap}", Source: "&{storage for x}", Edges: []string{"spill", "too large for stack"}},
			reason: gcdiff.ReasonTooLarge,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reason, known := testCase.flow.Reason()
			assert.True(t, known, "gc flow not categorised")
			assert.Equal(t, testCase.reason, reason)
		})
	}

	// A vocabulary neither classifier has been taught must come out
	// uncategorised and say so, rather than falling into a default category and
	// reading as a real answer.
	reason, known := gcdiff.ClassifyGocRule("a rule nobody has written yet")
	assert.False(t, known)
	assert.Equal(t, gcdiff.ReasonUncategorised, reason)
	unknownFlow := gcdiff.GCFlow{Dest: "{heap}", Source: "x", Edges: []string{"spill", "teleported"}}
	reason, known = unknownFlow.Reason()
	assert.False(t, known)
	assert.Equal(t, gcdiff.ReasonUncategorised, reason)
}

// TestGocFlagMParsesIntoSites reads a report shaped exactly as
// opt.WriteEscapeDiagnostics writes one, so the parser is covered without
// compiling anything.
func TestGocFlagMParsesIntoSites(t *testing.T) {
	const report = "prog.go:12:9: main_point does not escape\n" +
		"\tfront end: composite-literal in main.framed\n" +
		"prog.go:18:7: main_node escapes to heap\n" +
		"\tfront end: composite-literal in main.throughCall\n" +
		"\trule: assigned to the package-level variable keptPointer\n" +
		"\tfrom: p, declared at prog.go:8:18\n" +
		"\tat:   prog.go:8:30\n" +
		"?: main_anonymous escapes to heap\n" +
		"\tir pass: heap-alloc-candidate in main.main\n" +
		"\trule: store into non-local storage\n" +
		"other.go:3:3: main_elsewhere escapes to heap\n" +
		"\tir pass: heap-alloc-candidate in main.other\n" +
		"\trule: returned\n"

	parsed, err := gcdiff.ParseGocFlagM("prog.go", report)
	require.NoError(t, err)

	assert.Empty(t, parsed.Unknown)
	assert.Empty(t, parsed.UncategorisedRules)
	assert.Equal(t, 1, parsed.Positionless, "a site with no position cannot join and is counted")
	assert.Equal(t, 1, parsed.Foreign, "a site in another file is counted, not read")
	require.Len(t, parsed.Sites, 2)

	assert.Equal(t, gcdiff.GocSite{
		File: "prog.go", Line: 12, Col: 9,
		Placer: "front end", Site: "composite-literal", Func: "main.framed",
		Subject: "main_point", Placement: gcdiff.Frame,
	}, parsed.Sites[0], "a frame placement carries no rule, and so no category")

	assert.Equal(t, gcdiff.GocSite{
		File: "prog.go", Line: 18, Col: 7,
		Placer: "front end", Site: "composite-literal", Func: "main.throughCall",
		Subject: "main_node", Placement: gcdiff.Heap,
		Rule:   "assigned to the package-level variable keptPointer",
		Reason: gcdiff.ReasonStoredInObject,
		Chain:  []string{"p, declared at prog.go:8:18"},
		Use:    "prog.go:8:30",
	}, parsed.Sites[1])
}

// TestGCExplanationsParseTheFlowChain reads a -m=2 block as cmd/compile prints
// one.
func TestGCExplanationsParseTheFlowChain(t *testing.T) {
	const output = "# command-line-arguments\n" +
		"./prog.go:4:2: buffer escapes to heap in makeBuffer:\n" +
		"./prog.go:4:2:   flow: ~r0 ← &buffer:\n" +
		"./prog.go:4:2:     from &buffer (address-of) at ./prog.go:5:9\n" +
		"./prog.go:4:2:     from return &buffer (return) at ./prog.go:5:2\n" +
		"./prog.go:4:2: moved to heap: buffer\n" +
		"./prog.go:9:9: \"boom\" escapes to heap in main:\n" +
		"./prog.go:9:9:   flow: {heap} ← &{storage for \"boom\"}:\n" +
		"./prog.go:9:9:     from \"boom\" (spill) at ./prog.go:9:9\n" +
		"./prog.go:9:9:     from panic(\"boom\") (call parameter) at ./prog.go:9:8\n" +
		"/usr/lib/go/src/sync/once.go:1:1: x escapes to heap in sync.Once.Do:\n" +
		"/usr/lib/go/src/sync/once.go:1:1:   flow: {heap} ← &x:\n" +
		"./prog.go:9:9: \"boom\" escapes to heap\n"

	explanations, err := gcdiff.ParseGCExplanations("prog.go", output)
	require.NoError(t, err)

	assert.Equal(t, 2, explanations.Blocks)
	assert.Equal(t, 1, explanations.Foreign, "a block at a position outside the program is not this program's")

	// The subject on the explained block is the one "moved to heap: buffer"
	// names, so the two forms join with no special case.
	moved, found := explanations.Flows[gcdiff.ExplanationKey{Line: 4, Col: 2, Subject: "buffer"}]
	require.True(t, found)
	assert.Equal(t, "makeBuffer", moved.Func)
	assert.Equal(t, "~r0", moved.Dest)
	assert.Equal(t, []string{"address-of", "return"}, moved.Edges)
	reason, known := moved.Reason()
	assert.True(t, known)
	assert.Equal(t, gcdiff.ReasonReturned, reason)

	panicked, found := explanations.Flows[gcdiff.ExplanationKey{Line: 9, Col: 9, Subject: `"boom"`}]
	require.True(t, found)
	assert.Equal(t, "{heap}", panicked.Dest)
	reason, known = panicked.Reason()
	assert.True(t, known)
	assert.Equal(t, gcdiff.ReasonCallRetains, reason)
}

// TestGocFlagMStillParsesAsGCFlagM is the property the diagnostic was built for
// and this file now depends on from the other side: goc's -m is worded so that
// the strict cmd/compile parser reads it with nothing left over. If that ever
// stops being true, the two reports have diverged in shape and the reason join
// is comparing two different things.
func TestGocFlagMStillParsesAsGCFlagM(t *testing.T) {
	const report = "prog.go:12:9: main_point does not escape\n" +
		"\tfront end: composite-literal in main.framed\n" +
		"prog.go:18:7: main_node escapes to heap\n" +
		"\tir pass: heap-alloc-candidate in main.throughCall\n" +
		"\trule: store into non-local storage\n"

	asGC, err := gcdiff.ParseGCFlagM("prog.go", report)
	require.NoError(t, err)
	assert.Empty(t, asGC.Unknown)
	require.Len(t, asGC.Decisions, 2)
	assert.Equal(t, gcdiff.Frame, asGC.Decisions[0].Placement)
	assert.Equal(t, gcdiff.Heap, asGC.Decisions[1].Placement)

	asGoc, err := gcdiff.ParseGocFlagM("prog.go", report)
	require.NoError(t, err)
	require.Len(t, asGoc.Sites, 2)
	for index := range asGoc.Sites {
		assert.Equal(t, asGC.Decisions[index].Placement, asGoc.Sites[index].Placement,
			"the two parsers must agree about placement or the reason join is comparing\n"+
				"a decision against an explanation of a different decision")
	}
}
