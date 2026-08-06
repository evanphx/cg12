package goc_test

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
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

// measureGCDifferential asks for the differential to be run. It is opt-in for
// the same reason TestEscapeSummaryPromotionRate is: it depends on something
// outside this repository -- the host Go toolchain -- so it cannot be a
// required test without making the build machine's go version part of the
// contract. It is committed, and its output is committed, so the numbers can be
// diffed later instead of being reconstructed.
var measureGCDifferential = flag.Bool("escape-gc-differential", false,
	"compile every corpus program with the host Go toolchain and compare its escape decisions against the allocation census")

// updateGCDifferential rewrites the checked-in output from this run.
var updateGCDifferential = flag.Bool("update-escape-gc-differential", false,
	"rewrite testdata/escape_gc_differential.txt from this run")

const (
	gcDifferentialPath = "testdata/escape_gc_differential.txt"
	allocCensusPath    = "testdata/alloc_census_baseline.txt"
	corpusDir          = "testdata"
)

const gcDifferentialHeader = `# goc's allocation placement against cmd/compile's, per source line, over the
# whole corpus. Regenerate with
#
#     go test ./goc -run TestEscapeDifferentialAgainstGC \
#         -escape-gc-differential -update-escape-gc-differential
#
# and read the diff. The run needs a Go toolchain on PATH and takes a couple of
# minutes; it compiles nothing with goc, reading goc's side out of the
# already-committed alloc_census_baseline.txt instead, so it measures the census
# as committed rather than a second census built alongside it.
#
# What is being compared, and what the join key is, is documented at length in
# package internal/gcdiff. The short version: the key is (corpus file, source
# line), because columns and containing functions do not survive the trip
# between the two compilers and the source line does.
#
# -m's wording is not stable between Go releases, so this file records the host
# toolchain it was produced against. A diff against a run on a different release
# is not a change in goc.
#
`

// TestEscapeDifferentialAgainstGC measures goc's escape analysis against the
// reference implementation's, which is the only comparison that can say whether
// goc's is any good.
//
// Everything measured about goc's escape analysis before this compared two of
// goc's own analyses to each other -- the AST walk against the summary-fed IR
// pass -- which establishes only which of the two is better. cmd/compile is
// installed on the build machine and will print its escape decisions for
// -gcflags=-m, so this test asks it about every corpus program and joins the
// answers against the committed allocation census.
//
// # The two directions are not the same news
//
// gc frames it and goc heaps it: goc is pessimistic. Each line costs an
// allocation the reference implementation does not pay, and nothing else.
//
// goc frames it and gc heaps it: goc is permissive, and every one of those
// lines is a candidate correctness bug -- a frame address that may outlive its
// frame, which none of this tree's own instruments caught. goc can legitimately
// be more precise than gc here, because it compiles the whole program from
// source where gc is limited to what fits in export-data tags, so the set needs
// triage rather than a count. What it must never be is unexamined.
//
// # Why it asserts almost nothing
//
// The output is a baseline to be diffed, not a threshold to be passed. There is
// no number of disagreements that is correct, exactly as there is no allocation
// census that is correct; both files exist so that a person looks at what
// moved. The one thing this test does assert is that the parser understood
// every -m diagnostic it saw, because a message it silently skipped would
// remove a decision from one side of the matrix and read as agreement.
func TestEscapeDifferentialAgainstGC(t *testing.T) {
	t.Parallel()
	if !*measureGCDifferential && !*updateGCDifferential {
		t.Skip("pass -escape-gc-differential to build the corpus with the host Go toolchain and compare")
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
	result := gcdiff.Join(census, drops, programs, gcdiff.Options{})

	t.Logf("compared %d of %d corpus programs, %d census rows, %d gc decisions",
		result.Coverage.Compared, result.Coverage.Programs, result.Coverage.CensusRows, result.Coverage.GCDecisions)
	t.Logf("permissive (gc heaps, goc does not): %d lines", len(result.Permissive()))
	t.Logf("pessimistic (goc heaps, gc does not): %d lines", len(result.Pessimistic()))

	rendered := gcdiff.Render(gcDifferentialHeader, goVersion, result)
	if *updateGCDifferential {
		require.NoError(t, os.WriteFile(gcDifferentialPath, []byte(rendered), 0o644))
		t.Skip("output rewritten; rerun without -update-escape-gc-differential to check it")
	}

	assert.Empty(t, result.Coverage.UnknownGCDiagnostics,
		"the -m parser did not recognise a diagnostic. A message it skips is a decision missing\n"+
			"from gc's side of the matrix, which reads as agreement. Teach internal/gcdiff about it.")

	accepted, err := os.ReadFile(gcDifferentialPath)
	require.NoError(t, err)
	assert.Equal(t, string(accepted), rendered,
		"the differential moved. If the host toolchain is not the one recorded in the file,\n"+
			"that is the whole explanation and the fix is to rerun with\n"+
			"-update-escape-gc-differential. Otherwise something in goc changed where an\n"+
			"object is allocated, and the diff says which lines.")
}

// buildCorpusWithHostToolchain compiles every corpus program with the host go
// and parses the escape decisions it printed.
//
// Every program is reported, including the ones that do not build: a program
// the host toolchain rejects has to appear in the coverage numbers as a
// program that was not compared, or it silently shrinks the denominator and
// the comparison looks more complete than it is.
func buildCorpusWithHostToolchain(t *testing.T, paths []string) []gcdiff.Program {
	t.Helper()

	// One scratch module holding every program, each built on its own by naming
	// its file: `go build prog.go` compiles only the files named, so 385
	// package mains can share a directory without colliding.
	scratch := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(scratch, "go.mod"),
		[]byte("module gcdiffcorpus\n\ngo 1.21\n"), 0o644))

	programs := make([]gcdiff.Program, len(paths))
	for index, path := range paths {
		source, err := os.ReadFile(path)
		require.NoError(t, err)
		name := filepath.Base(path)
		require.NoError(t, os.WriteFile(filepath.Join(scratch, name), source, 0o644))
		programs[index] = gcdiff.Program{Name: name, Source: strings.Split(string(source), "\n")}
	}

	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	work := make(chan int)
	var working sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		working.Add(1)
		go func() {
			defer working.Done()
			for index := range work {
				program := &programs[index]
				report, buildError := runGCFlagM(scratch, program.Name)
				if buildError != "" {
					program.BuildError = buildError
					continue
				}
				program.Report = report
			}
		}()
	}
	for index := range programs {
		work <- index
	}
	close(work)
	working.Wait()
	return programs
}

// runGCFlagM builds one program and returns what -m said, or the reason it
// would not build.
func runGCFlagM(dir, program string) (*gcdiff.GCReport, string) {
	command := exec.Command("go", "build", "-o", os.DevNull, "-gcflags=-m", program)
	command.Dir = dir
	// The build cache is shared with everything else on the machine, which is
	// what makes a 385-program sweep take a couple of minutes rather than an
	// hour. Nothing here writes to the module cache.
	command.Env = append(os.Environ(), "GOFLAGS=")
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, buildFailureReason(string(output), err)
	}
	report, parseError := gcdiff.ParseGCFlagM(program, string(output))
	if parseError != nil {
		return nil, parseError.Error()
	}
	return &report, ""
}

// buildFailureReason reduces a build failure to one line, since the coverage
// table wants a reason and not a transcript.
func buildFailureReason(output string, err error) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		// "# command-line-arguments" and "package command-line-arguments" are
		// the banners go build puts above the real diagnostic.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "package command-line-arguments") {
			continue
		}
		return trimmed
	}
	return err.Error()
}

func hostGoVersion(t *testing.T) string {
	t.Helper()

	output, err := exec.Command("go", "version").Output()
	require.NoError(t, err, "the differential needs a Go toolchain on PATH")
	return strings.TrimSpace(string(output))
}

// differentialProgram names one corpus program to explain rather than measure.
var differentialProgram = flag.String("escape-gc-differential-program", "",
	"compile one program with goc and print every allocation decision in it beside cmd/compile's, for triaging a line the differential flagged")

// TestEscapeDifferentialProgram explains one program's disagreements instead of
// counting the whole corpus's.
//
// The differential's join is by source line, which is all the two compilers
// share, and a line is a coarse unit: a line carrying both a frame placement
// and a heap one says nothing on its own about which object is which. This
// prints what the line-level view had to fold away -- every decision goc's
// lowering recorded for the program, including the heap placements the loop
// rule forced, and every frame address opt.FrameEscapes can prove the program
// publishes -- beside the -m output for the same file. It is the tool for
// answering "is this line a real hole", which is the only question the corpus
// number cannot answer for itself.
func TestEscapeDifferentialProgram(t *testing.T) {
	t.Parallel()
	if *differentialProgram == "" {
		t.Skip("pass -escape-gc-differential-program=testdata/x.go to explain one program")
	}

	source, err := os.ReadFile(*differentialProgram)
	require.NoError(t, err)
	module, err := goc.CompileExecutable(*differentialProgram, source)
	require.NoError(t, err)

	name := filepath.Base(*differentialProgram)
	t.Logf("== goc allocation decisions in %s", name)
	for _, decision := range module.AllocDecisions {
		if filepath.Base(module.FileName(decision.Pos.File)) != name {
			continue
		}
		note := ""
		if decision.BlockedByLoop {
			note = "  (heap forced by the loop rule)"
		}
		t.Logf("  %d:%d\t%s\t%s\t%s\t%s%s", decision.Pos.Line, decision.Pos.Col,
			decision.Placement, decision.Allocator, opt.AllocationTypeName(decision.Type), decision.Func, note)
	}

	t.Logf("== goc census rows in %s", name)
	for _, allocation := range opt.AllocationCensus(module) {
		if filepath.Base(allocation.File) != name {
			continue
		}
		t.Logf("  %s", allocation.Key())
	}

	// A census row the front end gave no position is invisible to the
	// differential, which joins on position, and there are 5 863 of them over
	// the corpus. Some of them belong to the program under triage, and one of
	// them is often the answer: an object allocated by lowering rather than by
	// a source expression carries no position, so a line can look like it has
	// no heap allocation when the heap allocation is simply anonymous. Those
	// are printed here, identified by the function they are in.
	t.Logf("== positionless census rows in this program's own functions")
	for _, allocation := range opt.AllocationCensus(module) {
		if allocation.Pos.Valid() || !strings.HasPrefix(allocation.Func, "main.") {
			continue
		}
		t.Logf("  %s", allocation.Key())
	}

	t.Logf("== frame addresses %s publishes past their frame", name)
	for _, escape := range opt.FrameEscapes(module) {
		if filepath.Base(escape.File) != name {
			continue
		}
		t.Logf("  %s", escape.Key())
	}

	scratch := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(scratch, "go.mod"),
		[]byte("module gcdiffprobe\n\ngo 1.21\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(scratch, name), source, 0o644))
	report, buildError := runGCFlagM(scratch, name)
	if buildError != "" {
		t.Logf("== the host toolchain would not build %s: %s", name, buildError)
		return
	}
	t.Logf("== cmd/compile -m decisions in %s (%s)", name, hostGoVersion(t))
	for _, decision := range report.Decisions {
		note := ""
		if decision.Inlined {
			note = "  (a copy of an allocation written elsewhere, inlined here)"
		}
		t.Logf("  %d:%d\t%s\t%s\t%s%s", decision.Line, decision.Col, decision.Placement, decision.Kind, decision.Subject, note)
	}
}
