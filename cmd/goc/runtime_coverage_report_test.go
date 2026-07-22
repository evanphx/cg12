package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeCoverageClassifications(t *testing.T) {
	classifications := &runtimeCoverageClassificationFile{
		Version: 1,
		GOOS:    "linux",
		GOARCH:  "arm64",
		Rules: []runtimeCoverageClassificationRule{
			{
				File:           "runtime/malloc_generated.go",
				Classification: runtimeCoverageRequired,
				Reason:         "allocation fast path",
			},
			{
				File:           "runtime/trace*.go",
				Function:       "runtime.traceWriter*",
				Classification: runtimeCoverageRareRequired,
				Reason:         "trace writer path",
			},
		},
	}
	report := runtimeCoverageReport{
		MissingFunctions: []runtimeCoverageMissingFunction{
			{Name: "runtime.mallocgcSmallScanNoHeader", File: "runtime/malloc_generated.go", Line: 42},
			{Name: "runtime.traceWriter.flush", File: "runtime/tracebuf.go", Line: 100},
			{Name: "runtime.runqsteal", File: "runtime/proc.go", Line: 7000},
		},
	}

	if err := applyRuntimeCoverageClassifications(&report, classifications); err != nil {
		t.Fatal(err)
	}
	if got := report.MissingFunctions[0].Classification; got != runtimeCoverageRequired {
		t.Fatalf("malloc classification = %q, want %q", got, runtimeCoverageRequired)
	}
	if got := report.MissingFunctions[1].Classification; got != runtimeCoverageRareRequired {
		t.Fatalf("trace classification = %q, want %q", got, runtimeCoverageRareRequired)
	}
	if got := report.MissingFunctions[2].Classification; got != runtimeCoverageUnknown {
		t.Fatalf("scheduler classification = %q, want %q", got, runtimeCoverageUnknown)
	}
	if report.Summary.ClassifiedMissingFunctions != 2 || report.Summary.UnknownMissingFunctions != 1 {
		t.Fatalf("classification summary = %+v", report.Summary)
	}
}

func TestCheckedRuntimeCoverageClassifications(t *testing.T) {
	classifications, err := readRuntimeCoverageClassifications(
		defaultRuntimeCoverageClassifications(),
		"linux",
		"arm64",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(classifications.Rules) == 0 {
		t.Fatal("checked runtime coverage classifications contain no rules")
	}
}

func TestRuntimeCoverageClassificationsRejectAmbiguousRules(t *testing.T) {
	classifications := &runtimeCoverageClassificationFile{
		Rules: []runtimeCoverageClassificationRule{
			{
				File:           "runtime/*.go",
				Classification: runtimeCoverageRequired,
				Reason:         "broad rule",
			},
			{
				File:           "runtime/proc.go",
				Classification: runtimeCoverageRareRequired,
				Reason:         "specific rule",
			},
		},
	}
	function := runtimeCoverageMissingFunction{
		Name: "runtime.runqsteal",
		File: "runtime/proc.go",
		Line: 7000,
	}

	_, _, err := classifyRuntimeCoverageFunction(function, classifications)
	if err == nil || !strings.Contains(err.Error(), "matches classification rules 0 and 1") {
		t.Fatalf("ambiguous classification error = %v", err)
	}
}

func TestCompareRuntimeCoverageReports(t *testing.T) {
	baseline := runtimeCoverageTestReport()
	current := runtimeCoverageTestReport()
	current.Summary.CoveredPrograms = 0
	current.Summary.UnexpectedFailures = 1
	current.MissingFunctions = []runtimeCoverageMissingFunction{
		{
			Name:           "runtime.first",
			File:           "runtime/a.go",
			Line:           10,
			Reason:         "not executed",
			Classification: runtimeCoverageUnknown,
		},
		{
			Name:           "runtime.second",
			File:           "runtime/b.go",
			Line:           20,
			Reason:         "not executed",
			Classification: runtimeCoverageUnknown,
		},
	}
	current.UncoveredBlocks = []runtimeCoverageBlockReport{
		{Function: "runtime.first", File: "runtime/a.go", Line: 11, Block: 1},
		{Function: "runtime.second", File: "runtime/b.go", Line: 21, Block: 2},
	}
	current.ExecutedFunctions = []runtimeCoverageFunctionDiff{
		{Name: "runtime.third", File: "runtime/c.go", Line: 30},
	}
	current.ExecutedBlocks = []runtimeCoverageBlockReport{
		{Function: "runtime.third", File: "runtime/c.go", Line: 31, Block: 1},
	}
	current.Programs[0].Error = "run failed"
	current.Programs[0].RunOutcome = "failed"
	current.Programs[0].CoverageOutcome = "missing"

	difference, err := compareRuntimeCoverageReports(baseline, current)
	if err != nil {
		t.Fatal(err)
	}
	if len(difference.GainedFunctions) != 1 || len(difference.LostFunctions) != 1 {
		t.Fatalf("function difference = +%v -%v", difference.GainedFunctions, difference.LostFunctions)
	}
	if len(difference.GainedBlocks) != 1 || len(difference.LostBlocks) != 1 {
		t.Fatalf("block difference = +%v -%v", difference.GainedBlocks, difference.LostBlocks)
	}
	if len(difference.RegressedPrograms) != 1 || difference.RegressedPrograms[0] != "scheduler/work-steal" {
		t.Fatalf("regressed programs = %v", difference.RegressedPrograms)
	}
	if len(difference.NewMissingCoveragePrograms) != 1 {
		t.Fatalf("new missing coverage programs = %v", difference.NewMissingCoveragePrograms)
	}
	if !runtimeCoverageRegressed(difference) {
		t.Fatal("comparison did not report a regression")
	}
}

func TestCompareRuntimeCoverageReportsRejectsSourceDrift(t *testing.T) {
	baseline := runtimeCoverageTestReport()
	current := runtimeCoverageTestReport()
	current.RuntimeSourceID = "different-runtime"

	_, err := compareRuntimeCoverageReports(baseline, current)
	if err == nil || !strings.Contains(err.Error(), "runtime source differs") {
		t.Fatalf("source drift error = %v", err)
	}
}

func TestRuntimeCoverageDiffCommand(t *testing.T) {
	directory := t.TempDir()
	baselinePath := filepath.Join(directory, "baseline.json")
	currentPath := filepath.Join(directory, "current.json")
	writeRuntimeCoverageTestReport(t, baselinePath, runtimeCoverageTestReport())
	writeRuntimeCoverageTestReport(t, currentPath, runtimeCoverageTestReport())

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runtimeCoverageDiffCommand(
		[]string{"-fail-on-regression", baselinePath, currentPath},
		&stdout,
		&stderr,
	)
	if exitCode != 0 {
		t.Fatalf("runtime-cover-diff exit = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "executed functions: 1 -> 1 (+0)") {
		t.Fatalf("runtime-cover-diff output:\n%s", stdout.String())
	}
}

func runtimeCoverageTestReport() runtimeCoverageReport {
	return runtimeCoverageReport{
		Version:         runtimeCoverageReportVersion,
		GOOS:            "linux",
		GOARCH:          "arm64",
		RuntimeSourceID: "test-runtime",
		Summary: runtimeCoverageSummary{
			Programs:                1,
			CoveredPrograms:         1,
			ActiveFunctions:         3,
			CompiledFunctions:       3,
			ExecutedFunctions:       1,
			CompiledBlocks:          3,
			ExecutedBlocks:          1,
			UnknownMissingFunctions: 2,
			UnexpectedFailures:      0,
		},
		Programs: []runtimeCoverageProgramReport{
			{
				Category:        "scheduler",
				Name:            "work-steal",
				Expectation:     "must-pass",
				CompileOutcome:  "passed",
				RunOutcome:      "passed",
				CoverageOutcome: "collected",
			},
		},
		ExecutedFunctions: []runtimeCoverageFunctionDiff{
			{Name: "runtime.first", File: "runtime/a.go", Line: 10},
		},
		MissingFunctions: []runtimeCoverageMissingFunction{
			{
				Name:           "runtime.second",
				File:           "runtime/b.go",
				Line:           20,
				Reason:         "not executed",
				Classification: runtimeCoverageUnknown,
			},
			{
				Name:           "runtime.third",
				File:           "runtime/c.go",
				Line:           30,
				Reason:         "not executed",
				Classification: runtimeCoverageUnknown,
			},
		},
		ExecutedBlocks: []runtimeCoverageBlockReport{
			{Function: "runtime.first", File: "runtime/a.go", Line: 11, Block: 1},
		},
		UncoveredBlocks: []runtimeCoverageBlockReport{
			{Function: "runtime.second", File: "runtime/b.go", Line: 21, Block: 2},
			{Function: "runtime.third", File: "runtime/c.go", Line: 31, Block: 1},
		},
	}
}

func writeRuntimeCoverageTestReport(t *testing.T, filename string, report runtimeCoverageReport) {
	t.Helper()
	content, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, content, 0o644); err != nil {
		t.Fatal(err)
	}
}
