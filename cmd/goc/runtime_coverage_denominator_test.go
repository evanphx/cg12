package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/evanphx/cg12/goc"
)

// The capability matrix is the corpus denominator. These tests keep the matrix,
// the coverage report, and the accepted baseline reconciled with each other so
// none of the three can shrink without a recorded reason.

func TestRuntimeCapabilityMatrixIsWellFormed(t *testing.T) {
	capabilities := runtimeCapabilities()
	require.NotEmpty(t, capabilities)

	seen := make(map[string]bool, len(capabilities))
	for _, capability := range capabilities {
		name := capability.category + "/" + capability.name
		require.NotEmpty(t, capability.category, "capability %q has no category", name)
		require.NotEmpty(t, capability.name, "capability %q has no name", name)
		require.NotEmpty(t, capability.source, "capability %q has no source", name)
		require.False(t, seen[name], "capability %q is registered twice", name)
		seen[name] = true

		source := filepath.Join("..", "..", "goc", "testdata", capability.source)
		_, err := os.Stat(source)
		require.NoError(t, err, "capability %q source is missing", name)

		if capability.expectation == runtimeCapabilityExpectedFailure {
			require.Equal(
				t,
				runtimeCapabilityTerminatesAbnormally,
				capability.termination,
				"capability %q is expected to fail, so it must declare how it terminates",
				name,
			)
		}
		if capability.termination == runtimeCapabilityTerminatesAbnormally {
			require.NotEmpty(
				t,
				capability.terminationNote,
				"capability %q declares abnormal termination without a reason",
				name,
			)
			// A must-pass capability has to exit cleanly, so declaring it
			// abnormal is a contradiction. Rejecting it also closes the only
			// route by which a lost packet could be silenced as
			// expected-unavailable without the capability being reclassified.
			require.NotEqual(
				t,
				runtimeCapabilityMustPass,
				capability.expectation,
				"capability %q must pass, so it cannot also be declared to terminate abnormally",
				name,
			)
		}
	}
}

func TestCheckedRuntimeCoverageBaselineDenominator(t *testing.T) {
	baseline, err := readRuntimeCoverageReport(filepath.Join("testdata", "runtime_coverage_linux_arm64.json"))
	require.NoError(t, err)
	pending, err := readRuntimeCoverageBaselinePending(filepath.Join("testdata", "runtime_coverage_baseline_pending.json"))
	require.NoError(t, err)
	require.Equal(t, "runtime_coverage_linux_arm64.json", pending.Baseline)

	matrix := make(map[string]bool)
	for _, capability := range runtimeCapabilities() {
		matrix[capability.category+"/"+capability.name] = true
	}
	covered := make(map[string]bool, len(baseline.Programs))
	for _, program := range baseline.Programs {
		covered[runtimeCoverageProgramName(program)] = true
	}
	recorded := make(map[string]bool, len(pending.Capabilities))
	for _, entry := range pending.Capabilities {
		require.NotEmpty(t, entry.Reason, "pending capability %q has no reason", entry.Capability)
		require.False(t, recorded[entry.Capability], "pending capability %q is listed twice", entry.Capability)
		recorded[entry.Capability] = true
		require.True(t, matrix[entry.Capability], "pending capability %q is not in the matrix", entry.Capability)
		require.False(t, covered[entry.Capability], "pending capability %q is already in the baseline", entry.Capability)
	}

	for name := range covered {
		require.True(t, matrix[name], "the baseline covers %q, which the matrix no longer holds", name)
	}
	for name := range matrix {
		if covered[name] {
			continue
		}
		require.True(
			t,
			recorded[name],
			"capability %q is in neither the accepted baseline nor testdata/runtime_coverage_baseline_pending.json;"+
				" record why the baseline does not cover it, or rerun and accept a new baseline",
			name,
		)
	}
	require.Equal(t, len(matrix), len(covered)+len(recorded), "matrix, baseline, and pending list do not add up")
}

func TestMissingRuntimeCoverageOutcomeIsAlwaysNamed(t *testing.T) {
	abnormal := runtimeCapability{
		category:        "defer-panic",
		name:            "panic-string-output",
		termination:     runtimeCapabilityTerminatesAbnormally,
		terminationNote: "the program deliberately panics without recovering",
	}
	undeclaredAbnormal := runtimeCapability{
		category:    "defer-panic",
		name:        "undocumented",
		termination: runtimeCapabilityTerminatesAbnormally,
	}
	ordinary := runtimeCapability{category: "gc", name: "cleanup-basic"}

	testCases := []struct {
		name            string
		capability      runtimeCapability
		runOutcome      string
		expectedOutcome string
		expectedReason  string
	}{
		{
			name:            "declared abnormal termination is classified",
			capability:      abnormal,
			runOutcome:      runtimeCoverageOutcomeFailed,
			expectedOutcome: runtimeCoverageOutcomeExpectedUnavailable,
			expectedReason:  "the program deliberately panics without recovering",
		},
		{
			name:            "abnormal termination without a note still names itself",
			capability:      undeclaredAbnormal,
			runOutcome:      runtimeCoverageOutcomeFailed,
			expectedOutcome: runtimeCoverageOutcomeExpectedUnavailable,
			expectedReason:  "the program is classified as terminating abnormally",
		},
		{
			name:            "a timeout names the kill that dropped the packet",
			capability:      ordinary,
			runOutcome:      runtimeCoverageOutcomeTimeout,
			expectedOutcome: runtimeCoverageOutcomeMissing,
			expectedReason:  "the process was killed at its 30s timeout before the coverage packet was written",
		},
		{
			name:            "any other absence reports the extraction error",
			capability:      ordinary,
			runOutcome:      runtimeCoverageOutcomePassed,
			expectedOutcome: runtimeCoverageOutcomeMissing,
			expectedReason:  "goc: runtime coverage packet was not written",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			extractionErr := fmt.Errorf("goc: runtime coverage packet was not written")
			outcome, reason := missingRuntimeCoverageOutcome(
				testCase.capability,
				testCase.runOutcome,
				30*time.Second,
				extractionErr,
			)
			require.Equal(t, testCase.expectedOutcome, outcome)
			require.Equal(t, testCase.expectedReason, reason)
			require.NotEmpty(t, reason, "a missing packet must always carry a reason")
		})
	}
}

func TestRuntimeCorpusCoverageRecordsAnOutcomeForEveryCapability(t *testing.T) {
	withRuntimeCoverageProfile(t)

	capabilities := []runtimeCapability{
		{category: "gc", name: "collected", source: "a.go"},
		{category: "gc", name: "compile-failed", source: "b.go"},
		{category: "gc", name: "timed-out", source: "c.go"},
		{
			category:        "defer-panic",
			name:            "uncaught-panic",
			source:          "d.go",
			expectation:     runtimeCapabilityExpectedFailure,
			termination:     runtimeCapabilityTerminatesAbnormally,
			terminationNote: "the program deliberately panics without recovering",
		},
		{category: "stdlib-netpoll", name: "tcp-echo", source: "e.go", requiresAFINET: true},
	}
	results := []runtimeCapabilityResult{
		{
			compileOutcome:  runtimeCoverageOutcomePassed,
			runOutcome:      runtimeCoverageOutcomePassed,
			coverageOutcome: runtimeCoverageOutcomeCollected,
			runAttempts:     1,
			coverage:        runtimeCoverageTestPacket(),
		},
		{
			compileOutcome:  runtimeCoverageOutcomeFailed,
			runOutcome:      runtimeCoverageOutcomeNotRun,
			coverageOutcome: runtimeCoverageOutcomeNotRun,
			coverageReason:  "compilation failed, so the program was never executed",
			err:             fmt.Errorf("compile failed: exit status 2"),
		},
		{
			compileOutcome:  runtimeCoverageOutcomePassed,
			runOutcome:      runtimeCoverageOutcomeTimeout,
			coverageOutcome: runtimeCoverageOutcomeMissing,
			coverageReason:  "the process was killed at its 30s timeout before the coverage packet was written",
			runAttempts:     1,
			err:             fmt.Errorf("context deadline exceeded"),
		},
		{
			compileOutcome:  runtimeCoverageOutcomePassed,
			runOutcome:      runtimeCoverageOutcomeFailed,
			coverageOutcome: runtimeCoverageOutcomeExpectedUnavailable,
			coverageReason:  "the program deliberately panics without recovering",
			runAttempts:     1,
			err:             fmt.Errorf("exit status 2"),
		},
		skippedRuntimeCapabilityResult("AF_INET sockets unavailable in this execution environment"),
	}

	coverage := newRuntimeCorpusCoverage()
	coverage.expect(capabilities)
	for index, capability := range capabilities {
		coverage.add(capability, results[index])
	}
	require.Empty(t, coverage.unreportedPrograms())
	require.Empty(t, coverage.collectionErrors)

	report := coverage.report()
	require.Equal(t, len(capabilities), report.Summary.MatrixCapabilities)
	require.Equal(t, len(capabilities), report.Summary.Programs)
	require.Len(t, report.Programs, len(capabilities))
	require.Equal(t, 1, report.Summary.CoveredPrograms)
	require.Equal(t, 1, report.Summary.CompileFailures)
	require.Equal(t, 1, report.Summary.RunTimeouts)
	require.Equal(t, 1, report.Summary.SkippedPrograms)
	require.Equal(t, 1, report.Summary.ExpectedUnavailableCoveragePrograms)
	require.Equal(t, 4, report.Summary.MissingCoveragePrograms)

	for _, program := range report.Programs {
		name := runtimeCoverageProgramName(program)
		require.NotEmpty(t, program.CompileOutcome, "program %s has no compile outcome", name)
		require.NotEmpty(t, program.RunOutcome, "program %s has no run outcome", name)
		require.NotEmpty(t, program.CoverageOutcome, "program %s has no coverage outcome", name)
		if program.CoverageOutcome != runtimeCoverageOutcomeCollected {
			require.NotEmpty(t, program.CoverageReason, "program %s lost its packet without a reason", name)
		}
	}

	skipped := runtimeCoverageProgramSet(report.Programs)["stdlib-netpoll/tcp-echo"]
	require.Equal(t, runtimeCoverageOutcomeSkipped, skipped.CompileOutcome)
	require.Equal(t, "AF_INET sockets unavailable in this execution environment", skipped.SkipReason)

	panicking := runtimeCoverageProgramSet(report.Programs)["defer-panic/uncaught-panic"]
	require.Equal(t, runtimeCoverageTerminationAbnormal, panicking.Termination)

	require.NoError(t, validateRuntimeCoverageReport(report))
}

func TestRuntimeCorpusCoverageDetectsACapabilityThatReportsNothing(t *testing.T) {
	withRuntimeCoverageProfile(t)

	capabilities := []runtimeCapability{
		{category: "gc", name: "reported", source: "a.go"},
		{category: "gc", name: "silent", source: "b.go"},
	}
	coverage := newRuntimeCorpusCoverage()
	coverage.expect(capabilities)
	coverage.add(capabilities[0], runtimeCapabilityResult{
		compileOutcome:  runtimeCoverageOutcomePassed,
		runOutcome:      runtimeCoverageOutcomePassed,
		coverageOutcome: runtimeCoverageOutcomeCollected,
		runAttempts:     1,
		coverage:        runtimeCoverageTestPacket(),
	})

	require.Equal(t, []string{"gc/silent"}, coverage.unreportedPrograms())
}

// A capability that reports nothing is the one case where the collector could
// quietly publish a smaller corpus. It must instead keep the row, name the
// absence, and still produce a valid report a reviewer can diff.
func TestRuntimeCorpusCoverageKeepsAndNamesACapabilityThatReportsNothing(t *testing.T) {
	withRuntimeCoverageProfile(t)

	capabilities := []runtimeCapability{
		{category: "gc", name: "reported", source: "a.go"},
		{category: "gc", name: "silent", source: "b.go"},
	}
	coverage := newRuntimeCorpusCoverage()
	coverage.expect(capabilities)
	coverage.add(capabilities[0], runtimeCapabilityResult{
		compileOutcome:  runtimeCoverageOutcomePassed,
		runOutcome:      runtimeCoverageOutcomePassed,
		coverageOutcome: runtimeCoverageOutcomeCollected,
		runAttempts:     1,
		coverage:        runtimeCoverageTestPacket(),
	})

	coverage.recordUnreportedCapabilities()

	require.Empty(t, coverage.unreportedPrograms(), "the hole must have been filled with an explicit row")
	require.Len(t, coverage.collectionErrors, 1)
	require.Contains(
		t,
		coverage.collectionErrors[0],
		"gc/silent ran without reporting a compile, run, or coverage outcome",
	)

	report := coverage.report()
	require.Equal(t, len(capabilities), report.Summary.Programs)
	require.Equal(t, len(capabilities), report.Summary.MatrixCapabilities)
	require.Equal(t, 1, report.Summary.UnreportedPrograms)
	require.Equal(t, 1, report.Summary.MissingCoveragePrograms)

	silent := runtimeCoverageProgramSet(report.Programs)["gc/silent"]
	require.Equal(t, runtimeCoverageOutcomeUnreported, silent.CompileOutcome)
	require.Equal(t, runtimeCoverageOutcomeUnreported, silent.RunOutcome)
	require.Equal(t, runtimeCoverageOutcomeMissing, silent.CoverageOutcome)
	require.Equal(
		t,
		"the capability produced no outcome of its own: the run ended before it reported one",
		silent.CoverageReason,
	)
	require.Equal(t, 0, silent.RunAttempts)

	require.NoError(t, validateRuntimeCoverageReport(report))
}

// A packet the collector discards must not be recorded as collected coverage.
// Otherwise the program row and the corpus totals disagree about which programs
// contributed, which is the same silent shrinkage in a different disguise.
func TestRuntimeCorpusCoverageNamesADiscardedPacket(t *testing.T) {
	testCases := []struct {
		name     string
		result   runtimeCapabilityResult
		outcome  string
		reason   string
		errorGot string
	}{
		{
			name: "a packet from a different runtime source is refused",
			result: runtimeCapabilityResult{
				compileOutcome:  runtimeCoverageOutcomePassed,
				runOutcome:      runtimeCoverageOutcomePassed,
				coverageOutcome: runtimeCoverageOutcomeCollected,
				runAttempts:     1,
				coverage:        runtimeCoverageTestPacketFor("a-different-copy-of-the-go-runtime"),
			},
			outcome:  runtimeCoverageOutcomeMissing,
			reason:   "returned a coverage packet for runtime source a-different-copy-of-the-go-runtime, but the corpus is collecting test-runtime",
			errorGot: "gc/drifted returned a coverage packet for runtime source",
		},
		{
			name: "a packet without a runtime source ID is refused",
			result: runtimeCapabilityResult{
				compileOutcome:  runtimeCoverageOutcomePassed,
				runOutcome:      runtimeCoverageOutcomePassed,
				coverageOutcome: runtimeCoverageOutcomeCollected,
				runAttempts:     1,
				coverage:        runtimeCoverageTestPacketFor(""),
			},
			outcome:  runtimeCoverageOutcomeMissing,
			reason:   "returned a coverage packet without a runtime source ID",
			errorGot: "gc/drifted returned a coverage packet without a runtime source ID",
		},
		{
			name: "a later run that lost its packet discards the earlier merge",
			result: runtimeCapabilityResult{
				compileOutcome:  runtimeCoverageOutcomePassed,
				runOutcome:      runtimeCoverageOutcomePassed,
				coverageOutcome: runtimeCoverageOutcomeMissing,
				coverageReason:  "goc: runtime coverage packet was not written",
				runAttempts:     2,
				coverage:        runtimeCoverageTestPacket(),
			},
			outcome: runtimeCoverageOutcomeMissing,
			reason:  "goc: runtime coverage packet was not written",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			withRuntimeCoverageProfile(t)

			anchor := runtimeCapability{category: "gc", name: "anchor", source: "a.go"}
			drifted := runtimeCapability{category: "gc", name: "drifted", source: "b.go"}
			coverage := newRuntimeCorpusCoverage()
			coverage.expect([]runtimeCapability{anchor, drifted})
			coverage.add(anchor, runtimeCapabilityResult{
				compileOutcome:  runtimeCoverageOutcomePassed,
				runOutcome:      runtimeCoverageOutcomePassed,
				coverageOutcome: runtimeCoverageOutcomeCollected,
				runAttempts:     1,
				coverage:        runtimeCoverageTestPacket(),
			})
			coverage.add(drifted, testCase.result)

			report := coverage.report()
			row := runtimeCoverageProgramSet(report.Programs)["gc/drifted"]
			require.Equal(t, testCase.outcome, row.CoverageOutcome)
			require.Equal(t, testCase.reason, row.CoverageReason)
			require.Equal(t, 1, report.Summary.CoveredPrograms, "only the anchor contributed coverage")
			if testCase.errorGot == "" {
				require.Empty(t, coverage.collectionErrors)
			} else {
				require.Len(t, coverage.collectionErrors, 1)
				require.Contains(t, coverage.collectionErrors[0], testCase.errorGot)
			}
			require.NoError(t, validateRuntimeCoverageReport(report))
		})
	}
}

func TestValidateRuntimeCoverageReportRejectsADuplicatedProgram(t *testing.T) {
	report := runtimeCoverageTestReport()
	report.Kind = runtimeCoverageReportBaseline
	// One capability reported twice while another reported nothing keeps the
	// program count right, so only an identity check can catch it.
	report.Programs = append(report.Programs, report.Programs[0])
	report.Summary.Programs = 2
	report.Summary.MatrixCapabilities = 2
	report.Summary.CoveredPrograms = 2
	report.Summary.RunAttempts = 2

	err := validateRuntimeCoverageReport(report)
	require.ErrorContains(t, err, "program scheduler/work-steal appears more than once")
}

func TestRuntimeCorpusCoverageRejectsADuplicateOutcome(t *testing.T) {
	withRuntimeCoverageProfile(t)

	capability := runtimeCapability{category: "gc", name: "reported", source: "a.go"}
	result := runtimeCapabilityResult{
		compileOutcome:  runtimeCoverageOutcomePassed,
		runOutcome:      runtimeCoverageOutcomePassed,
		coverageOutcome: runtimeCoverageOutcomeCollected,
		runAttempts:     1,
		coverage:        runtimeCoverageTestPacket(),
	}

	coverage := newRuntimeCorpusCoverage()
	coverage.expect([]runtimeCapability{capability})
	coverage.add(capability, result)
	coverage.add(capability, result)

	require.Len(t, coverage.collectionErrors, 1)
	require.Contains(t, coverage.collectionErrors[0], "gc/reported reported more than one coverage outcome")
}

func TestValidateRuntimeCoverageProgramOutcomes(t *testing.T) {
	testCases := []struct {
		name    string
		program runtimeCoverageProgramReport
		error   string
	}{
		{
			name: "a skipped capability keeps its reason",
			program: runtimeCoverageProgramReport{
				CompileOutcome:  runtimeCoverageOutcomeSkipped,
				RunOutcome:      runtimeCoverageOutcomeSkipped,
				CoverageOutcome: runtimeCoverageOutcomeSkipped,
				SkipReason:      "AF_INET sockets unavailable",
			},
		},
		{
			name: "a skipped capability without a reason is rejected",
			program: runtimeCoverageProgramReport{
				CompileOutcome:  runtimeCoverageOutcomeSkipped,
				RunOutcome:      runtimeCoverageOutcomeSkipped,
				CoverageOutcome: runtimeCoverageOutcomeSkipped,
			},
			error: "skipped capability has no recorded skip reason",
		},
		{
			name: "a skipped capability that claims run attempts is rejected",
			program: runtimeCoverageProgramReport{
				CompileOutcome:  runtimeCoverageOutcomeSkipped,
				RunOutcome:      runtimeCoverageOutcomeSkipped,
				CoverageOutcome: runtimeCoverageOutcomeSkipped,
				SkipReason:      "AF_INET sockets unavailable",
				RunAttempts:     1,
			},
			error: "skipped capability has run attempts",
		},
		{
			name: "expected-unavailable coverage requires a declared termination",
			program: runtimeCoverageProgramReport{
				CompileOutcome:  runtimeCoverageOutcomePassed,
				RunOutcome:      runtimeCoverageOutcomeFailed,
				CoverageOutcome: runtimeCoverageOutcomeExpectedUnavailable,
				RunAttempts:     1,
			},
			error: "coverage is expected-unavailable without a declared abnormal termination",
		},
		{
			name: "a declared abnormal termination may lose its packet",
			program: runtimeCoverageProgramReport{
				CompileOutcome:  runtimeCoverageOutcomePassed,
				RunOutcome:      runtimeCoverageOutcomeFailed,
				CoverageOutcome: runtimeCoverageOutcomeExpectedUnavailable,
				Termination:     runtimeCoverageTerminationAbnormal,
				RunAttempts:     1,
			},
		},
		{
			name: "an unnamed coverage outcome is rejected",
			program: runtimeCoverageProgramReport{
				CompileOutcome:  runtimeCoverageOutcomePassed,
				RunOutcome:      runtimeCoverageOutcomePassed,
				CoverageOutcome: "",
				RunAttempts:     1,
			},
			error: `invalid coverage outcome ""`,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateRuntimeCoverageProgram(testCase.program)
			if testCase.error == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, testCase.error)
		})
	}
}

func TestValidateRuntimeCoverageReportRequiresAReasonForEveryAbsentPacket(t *testing.T) {
	report := runtimeCoverageTestReport()
	report.Kind = runtimeCoverageReportBaseline
	report.Summary.MatrixCapabilities = 1
	report.Programs[0].RunOutcome = runtimeCoverageOutcomeTimeout
	report.Programs[0].CoverageOutcome = runtimeCoverageOutcomeMissing
	report.Summary.CoveredPrograms = 0
	report.Summary.RunTimeouts = 1
	report.Summary.MissingCoveragePrograms = 1

	err := validateRuntimeCoverageReport(report)
	require.ErrorContains(t, err, "without a recorded reason")

	report.Programs[0].CoverageReason = "the process was killed at its 30s timeout before the coverage packet was written"
	require.NoError(t, validateRuntimeCoverageReport(report))
}

func TestValidateRuntimeCoverageReportRejectsAPartialCorpus(t *testing.T) {
	report := runtimeCoverageTestReport()
	report.Kind = runtimeCoverageReportBaseline
	report.Summary.MatrixCapabilities = 2

	err := validateRuntimeCoverageReport(report)
	require.ErrorContains(t, err, "matrix holds 2 capabilities, but report contains 1 programs")
}

// withRuntimeCoverageProfile points the collector at a throwaway profile so the
// collection paths, which are inert without one, run during unit tests.
func withRuntimeCoverageProfile(t *testing.T) {
	t.Helper()
	previous := *runtimeCoverageProfile
	*runtimeCoverageProfile = filepath.Join(t.TempDir(), "coverage.json")
	t.Cleanup(func() {
		*runtimeCoverageProfile = previous
	})
}

func runtimeCoverageTestPacket() *runtimeCapabilityCoverageResult {
	return runtimeCoverageTestPacketFor("test-runtime")
}

func runtimeCoverageTestPacketFor(runtimeSourceID string) *runtimeCapabilityCoverageResult {
	return &runtimeCapabilityCoverageResult{
		metadata: &goc.RuntimeCoverage{
			RuntimeSourceID: runtimeSourceID,
			Functions: []goc.RuntimeCoverageFunction{
				{Name: "runtime.first", File: "runtime/proc.go", Line: 10, Compiled: true, Entry: 0},
			},
			Blocks: []goc.RuntimeCoverageBlock{
				{ID: 0, Function: "runtime.first", File: "runtime/proc.go", Line: 11, Block: 0},
			},
		},
		hits: []bool{true},
	}
}

// TestRuntimeCorpusCoverageRecordsConcurrentOutcomesDeterministically covers the
// collector's new obligation. The capability matrix's run phase records outcomes
// from several goroutines at once, so add has to be safe to call concurrently --
// and the report must not depend on the order they arrived in. A coverage report
// is a diffable artifact; rows that moved between runs would make every diff
// unreadable, so this asserts the encoded report is byte-identical across two
// independently scheduled recordings.
func TestRuntimeCorpusCoverageRecordsConcurrentOutcomesDeterministically(t *testing.T) {
	withRuntimeCoverageProfile(t)

	capabilities := make([]runtimeCapability, 0, 200)
	for index := 0; index < 200; index++ {
		capabilities = append(capabilities, runtimeCapability{
			category: fmt.Sprintf("category-%d", index%7),
			name:     fmt.Sprintf("capability-%03d", index),
			source:   fmt.Sprintf("program_%03d.go", index),
		})
	}

	record := func() string {
		coverage := newRuntimeCorpusCoverage()
		coverage.expect(capabilities)

		var recorded sync.WaitGroup
		for _, capability := range capabilities {
			capability := capability
			recorded.Add(1)
			go func() {
				defer recorded.Done()
				coverage.add(capability, runtimeCapabilityResult{
					compileOutcome:  runtimeCoverageOutcomePassed,
					runOutcome:      runtimeCoverageOutcomePassed,
					coverageOutcome: runtimeCoverageOutcomeCollected,
					runAttempts:     1,
					coverage:        runtimeCoverageTestPacket(),
				})
			}()
		}
		recorded.Wait()

		require.Empty(t, coverage.unreportedPrograms())
		require.Empty(t, coverage.collectionErrors)

		report := coverage.report()
		require.Len(t, report.Programs, len(capabilities))
		require.Equal(t, len(capabilities), report.Summary.CoveredPrograms)
		require.NoError(t, validateRuntimeCoverageReport(report))

		encoded, err := json.MarshalIndent(report, "", "  ")
		require.NoError(t, err)
		return string(encoded)
	}

	require.Equal(t, record(), record())
}
