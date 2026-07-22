package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"sort"
)

const runtimeCoverageReportVersion = 2

type runtimeCoverageReport struct {
	Version           int                              `json:"version"`
	GOOS              string                           `json:"goos"`
	GOARCH            string                           `json:"goarch"`
	RuntimeSourceID   string                           `json:"runtime_source_id"`
	Scope             string                           `json:"scope"`
	AssemblyFiles     []string                         `json:"uninstrumented_assembly_files"`
	Summary           runtimeCoverageSummary           `json:"summary"`
	Programs          []runtimeCoverageProgramReport   `json:"programs"`
	Files             []runtimeCoverageFileReport      `json:"files"`
	ExecutedFunctions []runtimeCoverageFunctionDiff    `json:"executed_functions"`
	MissingFunctions  []runtimeCoverageMissingFunction `json:"missing_functions"`
	ExecutedBlocks    []runtimeCoverageBlockReport     `json:"executed_blocks"`
	UncoveredBlocks   []runtimeCoverageBlockReport     `json:"uncovered_blocks"`
}

type runtimeCoverageSummary struct {
	Programs                   int     `json:"programs"`
	CoveredPrograms            int     `json:"covered_programs"`
	ActiveFunctions            int     `json:"active_functions"`
	CompiledFunctions          int     `json:"compiled_functions"`
	ExecutedFunctions          int     `json:"executed_functions"`
	FunctionCoverage           float64 `json:"function_coverage_percent"`
	CompiledBlocks             int     `json:"compiled_blocks"`
	ExecutedBlocks             int     `json:"executed_blocks"`
	BlockCoverage              float64 `json:"block_coverage_percent"`
	ClassifiedMissingFunctions int     `json:"classified_missing_functions"`
	UnknownMissingFunctions    int     `json:"unknown_missing_functions"`
	UnexpectedFailures         int     `json:"unexpected_failures"`
}

type runtimeCoverageFileReport struct {
	File              string `json:"file"`
	ActiveFunctions   int    `json:"active_functions"`
	CompiledFunctions int    `json:"compiled_functions"`
	ExecutedFunctions int    `json:"executed_functions"`
	CompiledBlocks    int    `json:"compiled_blocks"`
	ExecutedBlocks    int    `json:"executed_blocks"`
}

type runtimeCoverageMissingFunction struct {
	Name           string                        `json:"name"`
	File           string                        `json:"file"`
	Line           int                           `json:"line"`
	Reason         string                        `json:"reason"`
	Classification runtimeCoverageClassification `json:"classification"`
	Note           string                        `json:"note,omitempty"`
}

type runtimeCoverageBlockReport struct {
	Function string `json:"function"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Block    int    `json:"block"`
}

type runtimeCoverageProgramReport struct {
	Category        string `json:"category"`
	Name            string `json:"name"`
	Source          string `json:"source"`
	Expectation     string `json:"expectation"`
	CompileOutcome  string `json:"compile_outcome"`
	RunOutcome      string `json:"run_outcome"`
	CoverageOutcome string `json:"coverage_outcome"`
	Error           string `json:"error,omitempty"`
	CoverageError   string `json:"coverage_error,omitempty"`
	Output          string `json:"output,omitempty"`
}

type runtimeCoverageClassification string

const (
	runtimeCoverageRequired              runtimeCoverageClassification = "required"
	runtimeCoverageRareRequired          runtimeCoverageClassification = "rare-required"
	runtimeCoverageConfigurationDisabled runtimeCoverageClassification = "configuration-disabled"
	runtimeCoverageFatalOnly             runtimeCoverageClassification = "fatal-only"
	runtimeCoverageExternalIntegration   runtimeCoverageClassification = "external-integration"
	runtimeCoverageUnreachable           runtimeCoverageClassification = "unreachable"
	runtimeCoverageCompilerNotEmitted    runtimeCoverageClassification = "compiler-not-emitted"
	runtimeCoverageUnknown               runtimeCoverageClassification = "unknown"
)

type runtimeCoverageClassificationFile struct {
	Version int                                 `json:"version"`
	GOOS    string                              `json:"goos"`
	GOARCH  string                              `json:"goarch"`
	Rules   []runtimeCoverageClassificationRule `json:"rules"`
}

type runtimeCoverageClassificationRule struct {
	File           string                        `json:"file"`
	Function       string                        `json:"function,omitempty"`
	Classification runtimeCoverageClassification `json:"classification"`
	Reason         string                        `json:"reason"`
}

func readRuntimeCoverageClassifications(filename, goos, goarch string) (*runtimeCoverageClassificationFile, error) {
	content, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var classifications runtimeCoverageClassificationFile
	if err := json.Unmarshal(content, &classifications); err != nil {
		return nil, fmt.Errorf("decode runtime coverage classifications: %w", err)
	}
	if classifications.Version != 1 {
		return nil, fmt.Errorf("runtime coverage classifications version is %d, want 1", classifications.Version)
	}
	if classifications.GOOS != goos || classifications.GOARCH != goarch {
		return nil, fmt.Errorf(
			"runtime coverage classifications target is %s/%s, want %s/%s",
			classifications.GOOS,
			classifications.GOARCH,
			goos,
			goarch,
		)
	}
	for index, rule := range classifications.Rules {
		if rule.File == "" {
			return nil, fmt.Errorf("runtime coverage classification rule %d has no file pattern", index)
		}
		if _, err := pathpkg.Match(rule.File, "runtime/example.go"); err != nil {
			return nil, fmt.Errorf("runtime coverage classification rule %d has invalid file pattern: %w", index, err)
		}
		if rule.Function != "" {
			if _, err := pathpkg.Match(rule.Function, "runtime.example"); err != nil {
				return nil, fmt.Errorf("runtime coverage classification rule %d has invalid function pattern: %w", index, err)
			}
		}
		if !validRuntimeCoverageClassification(rule.Classification) {
			return nil, fmt.Errorf(
				"runtime coverage classification rule %d has invalid classification %q",
				index,
				rule.Classification,
			)
		}
		if rule.Classification == runtimeCoverageUnknown {
			return nil, fmt.Errorf("runtime coverage classification rule %d explicitly classifies a function as unknown", index)
		}
		if rule.Reason == "" {
			return nil, fmt.Errorf("runtime coverage classification rule %d has no reason", index)
		}
	}
	return &classifications, nil
}

func validRuntimeCoverageClassification(classification runtimeCoverageClassification) bool {
	switch classification {
	case runtimeCoverageRequired,
		runtimeCoverageRareRequired,
		runtimeCoverageConfigurationDisabled,
		runtimeCoverageFatalOnly,
		runtimeCoverageExternalIntegration,
		runtimeCoverageUnreachable,
		runtimeCoverageCompilerNotEmitted,
		runtimeCoverageUnknown:
		return true
	default:
		return false
	}
}

func classifyRuntimeCoverageFunction(
	function runtimeCoverageMissingFunction,
	classifications *runtimeCoverageClassificationFile,
) (runtimeCoverageClassification, string, error) {
	classification := runtimeCoverageUnknown
	reason := ""
	matchedRule := -1
	for index, rule := range classifications.Rules {
		fileMatches, err := pathpkg.Match(rule.File, function.File)
		if err != nil {
			return "", "", err
		}
		if !fileMatches {
			continue
		}
		if rule.Function != "" {
			functionMatches, err := pathpkg.Match(rule.Function, function.Name)
			if err != nil {
				return "", "", err
			}
			if !functionMatches {
				continue
			}
		}
		if matchedRule >= 0 {
			return "", "", fmt.Errorf(
				"runtime coverage function %s at %s:%d matches classification rules %d and %d",
				function.Name,
				function.File,
				function.Line,
				matchedRule,
				index,
			)
		}
		matchedRule = index
		classification = rule.Classification
		reason = rule.Reason
	}
	return classification, reason, nil
}

func applyRuntimeCoverageClassifications(
	report *runtimeCoverageReport,
	classifications *runtimeCoverageClassificationFile,
) error {
	report.Summary.ClassifiedMissingFunctions = 0
	report.Summary.UnknownMissingFunctions = 0
	for index := range report.MissingFunctions {
		function := &report.MissingFunctions[index]
		classification, reason, err := classifyRuntimeCoverageFunction(*function, classifications)
		if err != nil {
			return err
		}
		function.Classification = classification
		function.Note = reason
		if classification == runtimeCoverageUnknown {
			report.Summary.UnknownMissingFunctions++
		} else {
			report.Summary.ClassifiedMissingFunctions++
		}
	}
	return nil
}

type runtimeCoverageDiff struct {
	GOOS                       string                        `json:"goos"`
	GOARCH                     string                        `json:"goarch"`
	RuntimeSourceID            string                        `json:"runtime_source_id"`
	Baseline                   runtimeCoverageSummary        `json:"baseline"`
	Current                    runtimeCoverageSummary        `json:"current"`
	GainedFunctions            []runtimeCoverageFunctionDiff `json:"gained_functions"`
	LostFunctions              []runtimeCoverageFunctionDiff `json:"lost_functions"`
	GainedBlocks               []runtimeCoverageBlockReport  `json:"gained_blocks"`
	LostBlocks                 []runtimeCoverageBlockReport  `json:"lost_blocks"`
	RecoveredPrograms          []string                      `json:"recovered_programs"`
	RegressedPrograms          []string                      `json:"regressed_programs"`
	NewMissingCoveragePrograms []string                      `json:"new_missing_coverage_programs"`
}

type runtimeCoverageFunctionDiff struct {
	Name string `json:"name"`
	File string `json:"file"`
	Line int    `json:"line"`
}

func compareRuntimeCoverageReports(baseline, current runtimeCoverageReport) (runtimeCoverageDiff, error) {
	if err := validateRuntimeCoverageReport(baseline); err != nil {
		return runtimeCoverageDiff{}, fmt.Errorf("baseline report: %w", err)
	}
	if err := validateRuntimeCoverageReport(current); err != nil {
		return runtimeCoverageDiff{}, fmt.Errorf("current report: %w", err)
	}
	if baseline.GOOS != current.GOOS || baseline.GOARCH != current.GOARCH {
		return runtimeCoverageDiff{}, fmt.Errorf(
			"runtime coverage targets differ: baseline %s/%s, current %s/%s",
			baseline.GOOS,
			baseline.GOARCH,
			current.GOOS,
			current.GOARCH,
		)
	}
	if baseline.RuntimeSourceID != current.RuntimeSourceID {
		return runtimeCoverageDiff{}, fmt.Errorf(
			"runtime source differs: baseline %s, current %s",
			baseline.RuntimeSourceID,
			current.RuntimeSourceID,
		)
	}

	difference := runtimeCoverageDiff{
		GOOS:            current.GOOS,
		GOARCH:          current.GOARCH,
		RuntimeSourceID: current.RuntimeSourceID,
		Baseline:        baseline.Summary,
		Current:         current.Summary,
	}

	baselineExecutedFunctions := runtimeCoverageFunctionSet(baseline.ExecutedFunctions)
	currentExecutedFunctions := runtimeCoverageFunctionSet(current.ExecutedFunctions)
	for key := range currentExecutedFunctions {
		if !baselineExecutedFunctions[key] {
			difference.GainedFunctions = append(difference.GainedFunctions, key)
		}
	}
	for key := range baselineExecutedFunctions {
		if !currentExecutedFunctions[key] {
			difference.LostFunctions = append(difference.LostFunctions, key)
		}
	}

	baselineExecutedBlocks := runtimeCoverageBlockSet(baseline.ExecutedBlocks)
	currentExecutedBlocks := runtimeCoverageBlockSet(current.ExecutedBlocks)
	for block := range currentExecutedBlocks {
		if !baselineExecutedBlocks[block] {
			difference.GainedBlocks = append(difference.GainedBlocks, block)
		}
	}
	for block := range baselineExecutedBlocks {
		if !currentExecutedBlocks[block] {
			difference.LostBlocks = append(difference.LostBlocks, block)
		}
	}

	baselinePrograms := runtimeCoverageProgramSet(baseline.Programs)
	currentPrograms := runtimeCoverageProgramSet(current.Programs)
	for name, currentProgram := range currentPrograms {
		baselineProgram, found := baselinePrograms[name]
		if !found {
			continue
		}
		baselineFailed := runtimeCoverageProgramFailed(baselineProgram)
		currentFailed := runtimeCoverageProgramFailed(currentProgram)
		if baselineFailed && !currentFailed {
			difference.RecoveredPrograms = append(difference.RecoveredPrograms, name)
		}
		if !baselineFailed && currentFailed {
			difference.RegressedPrograms = append(difference.RegressedPrograms, name)
		}
		if baselineProgram.CoverageOutcome == "collected" && currentProgram.CoverageOutcome != "collected" {
			difference.NewMissingCoveragePrograms = append(difference.NewMissingCoveragePrograms, name)
		}
	}

	sortRuntimeCoverageDiff(&difference)
	return difference, nil
}

func validateRuntimeCoverageReport(report runtimeCoverageReport) error {
	if report.Version != runtimeCoverageReportVersion {
		return fmt.Errorf("version is %d, want %d", report.Version, runtimeCoverageReportVersion)
	}
	if report.GOOS == "" || report.GOARCH == "" {
		return errors.New("target is incomplete")
	}
	if report.RuntimeSourceID == "" {
		return errors.New("runtime_source_id is empty")
	}
	if report.Summary.Programs != len(report.Programs) {
		return fmt.Errorf("program count is %d, but report contains %d programs", report.Summary.Programs, len(report.Programs))
	}
	coveredPrograms := 0
	unexpectedFailures := 0
	for _, program := range report.Programs {
		if program.CoverageOutcome == "collected" {
			coveredPrograms++
		}
		if runtimeCoverageProgramFailed(program) {
			unexpectedFailures++
		}
	}
	if report.Summary.CoveredPrograms != coveredPrograms {
		return fmt.Errorf(
			"covered program count is %d, but report contains %d collected outcomes",
			report.Summary.CoveredPrograms,
			coveredPrograms,
		)
	}
	if report.Summary.UnexpectedFailures != unexpectedFailures {
		return fmt.Errorf(
			"unexpected failure count is %d, but report contains %d must-pass failures",
			report.Summary.UnexpectedFailures,
			unexpectedFailures,
		)
	}
	activeFunctions := len(report.ExecutedFunctions) + len(report.MissingFunctions)
	if report.Summary.ActiveFunctions != activeFunctions {
		return fmt.Errorf(
			"active function count is %d, but report contains %d function outcomes",
			report.Summary.ActiveFunctions,
			activeFunctions,
		)
	}
	if report.Summary.ExecutedFunctions != len(report.ExecutedFunctions) {
		return fmt.Errorf(
			"executed function count is %d, but report contains %d executed functions",
			report.Summary.ExecutedFunctions,
			len(report.ExecutedFunctions),
		)
	}
	compiledFunctions := len(report.ExecutedFunctions)
	classifiedMissingFunctions := 0
	unknownMissingFunctions := 0
	for _, function := range report.MissingFunctions {
		switch function.Reason {
		case "not executed":
			compiledFunctions++
		case "not compiled":
		default:
			return fmt.Errorf("function %s has invalid missing reason %q", function.Name, function.Reason)
		}
		if !validRuntimeCoverageClassification(function.Classification) {
			return fmt.Errorf("function %s has invalid classification %q", function.Name, function.Classification)
		}
		if function.Classification == runtimeCoverageUnknown {
			unknownMissingFunctions++
		} else {
			classifiedMissingFunctions++
		}
	}
	if report.Summary.CompiledFunctions != compiledFunctions {
		return fmt.Errorf(
			"compiled function count is %d, but report contains %d compiled function outcomes",
			report.Summary.CompiledFunctions,
			compiledFunctions,
		)
	}
	if report.Summary.ClassifiedMissingFunctions != classifiedMissingFunctions {
		return fmt.Errorf(
			"classified missing function count is %d, but report contains %d classifications",
			report.Summary.ClassifiedMissingFunctions,
			classifiedMissingFunctions,
		)
	}
	if report.Summary.UnknownMissingFunctions != unknownMissingFunctions {
		return fmt.Errorf(
			"unknown missing function count is %d, but report contains %d unknown classifications",
			report.Summary.UnknownMissingFunctions,
			unknownMissingFunctions,
		)
	}
	compiledBlocks := len(report.ExecutedBlocks) + len(report.UncoveredBlocks)
	if report.Summary.CompiledBlocks != compiledBlocks {
		return fmt.Errorf(
			"compiled block count is %d, but report contains %d block outcomes",
			report.Summary.CompiledBlocks,
			compiledBlocks,
		)
	}
	if report.Summary.ExecutedBlocks != len(report.ExecutedBlocks) {
		return fmt.Errorf(
			"executed block count is %d, but report contains %d executed blocks",
			report.Summary.ExecutedBlocks,
			len(report.ExecutedBlocks),
		)
	}
	return nil
}

func runtimeCoverageFunctionSet(functions []runtimeCoverageFunctionDiff) map[runtimeCoverageFunctionDiff]bool {
	set := make(map[runtimeCoverageFunctionDiff]bool, len(functions))
	for _, function := range functions {
		set[function] = true
	}
	return set
}

func runtimeCoverageBlockSet(blocks []runtimeCoverageBlockReport) map[runtimeCoverageBlockReport]bool {
	set := make(map[runtimeCoverageBlockReport]bool, len(blocks))
	for _, block := range blocks {
		set[block] = true
	}
	return set
}

func runtimeCoverageProgramSet(programs []runtimeCoverageProgramReport) map[string]runtimeCoverageProgramReport {
	set := make(map[string]runtimeCoverageProgramReport, len(programs))
	for _, program := range programs {
		set[runtimeCoverageProgramName(program)] = program
	}
	return set
}

func runtimeCoverageProgramName(program runtimeCoverageProgramReport) string {
	return program.Category + "/" + program.Name
}

func runtimeCoverageProgramFailed(program runtimeCoverageProgramReport) bool {
	return program.Expectation == "must-pass" && program.Error != ""
}

func sortRuntimeCoverageDiff(difference *runtimeCoverageDiff) {
	sort.Slice(difference.GainedFunctions, func(left, right int) bool {
		return runtimeCoverageFunctionDiffLess(difference.GainedFunctions[left], difference.GainedFunctions[right])
	})
	sort.Slice(difference.LostFunctions, func(left, right int) bool {
		return runtimeCoverageFunctionDiffLess(difference.LostFunctions[left], difference.LostFunctions[right])
	})
	sort.Slice(difference.GainedBlocks, func(left, right int) bool {
		return runtimeCoverageBlockReportLess(difference.GainedBlocks[left], difference.GainedBlocks[right])
	})
	sort.Slice(difference.LostBlocks, func(left, right int) bool {
		return runtimeCoverageBlockReportLess(difference.LostBlocks[left], difference.LostBlocks[right])
	})
	sort.Strings(difference.RecoveredPrograms)
	sort.Strings(difference.RegressedPrograms)
	sort.Strings(difference.NewMissingCoveragePrograms)
}

func runtimeCoverageFunctionDiffLess(left, right runtimeCoverageFunctionDiff) bool {
	if left.File != right.File {
		return left.File < right.File
	}
	if left.Line != right.Line {
		return left.Line < right.Line
	}
	return left.Name < right.Name
}

func runtimeCoverageBlockReportLess(left, right runtimeCoverageBlockReport) bool {
	if left.File != right.File {
		return left.File < right.File
	}
	if left.Line != right.Line {
		return left.Line < right.Line
	}
	if left.Function != right.Function {
		return left.Function < right.Function
	}
	return left.Block < right.Block
}

func runtimeCoverageDiffCommand(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("goc runtime-cover-diff", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write the complete comparison as JSON")
	failOnRegression := flags.Bool("fail-on-regression", false, "return failure when coverage or a must-pass program regresses")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: goc runtime-cover-diff [-json] [-fail-on-regression] baseline.json current.json")
		return 2
	}

	baseline, err := readRuntimeCoverageReport(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "goc: %v\n", err)
		return 1
	}
	current, err := readRuntimeCoverageReport(flags.Arg(1))
	if err != nil {
		fmt.Fprintf(stderr, "goc: %v\n", err)
		return 1
	}
	difference, err := compareRuntimeCoverageReports(baseline, current)
	if err != nil {
		fmt.Fprintf(stderr, "goc: compare runtime coverage: %v\n", err)
		return 1
	}

	if *jsonOutput {
		encoded, err := json.MarshalIndent(difference, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "goc: encode runtime coverage comparison: %v\n", err)
			return 1
		}
		encoded = append(encoded, '\n')
		if _, err := stdout.Write(encoded); err != nil {
			fmt.Fprintf(stderr, "goc: write runtime coverage comparison: %v\n", err)
			return 1
		}
	} else {
		writeRuntimeCoverageDiff(stdout, difference)
	}

	if *failOnRegression && runtimeCoverageRegressed(difference) {
		return 1
	}
	return 0
}

func readRuntimeCoverageReport(filename string) (runtimeCoverageReport, error) {
	content, err := os.ReadFile(filename)
	if err != nil {
		return runtimeCoverageReport{}, fmt.Errorf("read runtime coverage report %s: %w", filename, err)
	}
	var report runtimeCoverageReport
	if err := json.Unmarshal(content, &report); err != nil {
		return runtimeCoverageReport{}, fmt.Errorf("decode runtime coverage report %s: %w", filename, err)
	}
	return report, nil
}

func writeRuntimeCoverageDiff(output io.Writer, difference runtimeCoverageDiff) {
	fmt.Fprintf(output, "runtime coverage %s/%s source %s\n", difference.GOOS, difference.GOARCH, difference.RuntimeSourceID)
	writeRuntimeCoverageMetric(output, "covered programs", difference.Baseline.CoveredPrograms, difference.Current.CoveredPrograms)
	writeRuntimeCoverageMetric(output, "executed functions", difference.Baseline.ExecutedFunctions, difference.Current.ExecutedFunctions)
	writeRuntimeCoverageMetric(output, "executed blocks", difference.Baseline.ExecutedBlocks, difference.Current.ExecutedBlocks)
	writeRuntimeCoverageMetric(output, "unexpected failures", difference.Baseline.UnexpectedFailures, difference.Current.UnexpectedFailures)
	fmt.Fprintf(output, "gained functions: %d\n", len(difference.GainedFunctions))
	fmt.Fprintf(output, "lost functions: %d\n", len(difference.LostFunctions))
	fmt.Fprintf(output, "gained blocks: %d\n", len(difference.GainedBlocks))
	fmt.Fprintf(output, "lost blocks: %d\n", len(difference.LostBlocks))
	fmt.Fprintf(output, "recovered programs: %d\n", len(difference.RecoveredPrograms))
	fmt.Fprintf(output, "regressed programs: %d\n", len(difference.RegressedPrograms))
	fmt.Fprintf(output, "new missing coverage programs: %d\n", len(difference.NewMissingCoveragePrograms))
	for _, program := range difference.RegressedPrograms {
		fmt.Fprintf(output, "  regressed: %s\n", program)
	}
	for _, function := range difference.LostFunctions {
		fmt.Fprintf(output, "  lost function: %s (%s:%d)\n", function.Name, function.File, function.Line)
	}
}

func writeRuntimeCoverageMetric(output io.Writer, name string, baseline, current int) {
	fmt.Fprintf(output, "%s: %d -> %d (%+d)\n", name, baseline, current, current-baseline)
}

func runtimeCoverageRegressed(difference runtimeCoverageDiff) bool {
	return len(difference.LostFunctions) > 0 ||
		len(difference.LostBlocks) > 0 ||
		len(difference.RegressedPrograms) > 0 ||
		len(difference.NewMissingCoveragePrograms) > 0
}
