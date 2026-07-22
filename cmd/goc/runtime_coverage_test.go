package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/evanphx/cg12/goc"
)

var runtimeCoverageProfile = flag.String(
	"runtime-coverprofile",
	"",
	"write Linux/ARM64 runtime corpus coverage as JSON",
)

var runtimeCoverageClassifications = flag.String(
	"runtime-coverclassifications",
	defaultRuntimeCoverageClassifications(),
	"classify missing runtime functions using this JSON file",
)

var runtimeCoverageCollector = newRuntimeCorpusCoverage()

type runtimeCapabilityCoverageResult struct {
	metadata *goc.RuntimeCoverage
	hits     []bool
}

func readRuntimeCapabilityCoverage(metadataPath string, output []byte) (*runtimeCapabilityCoverageResult, []byte, error) {
	metadataJSON, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, output, fmt.Errorf("read runtime coverage metadata: %w", err)
	}
	var metadata goc.RuntimeCoverage
	if err := json.Unmarshal(metadataJSON, &metadata); err != nil {
		return nil, output, fmt.Errorf("decode runtime coverage metadata: %w", err)
	}
	result, err := goc.ExtractRuntimeCoverage(output, &metadata)
	if err != nil {
		return nil, output, err
	}
	return &runtimeCapabilityCoverageResult{
		metadata: &metadata,
		hits:     result.Hits,
	}, result.Output, nil
}

type runtimeFunctionKey struct {
	name string
	file string
	line int
}

type runtimeBlockKey struct {
	function string
	file     string
	line     int
	block    int
}

type runtimeCorpusCoverage struct {
	programs          int
	coveredPrograms   int
	runtimeSourceID   string
	collectionErrors  []string
	assemblyFiles     map[string]bool
	programResults    []runtimeCoverageProgramReport
	activeFunctions   map[runtimeFunctionKey]goc.RuntimeCoverageFunction
	compiledFunctions map[runtimeFunctionKey]bool
	executedFunctions map[runtimeFunctionKey]bool
	compiledBlocks    map[runtimeBlockKey]goc.RuntimeCoverageBlock
	executedBlocks    map[runtimeBlockKey]bool
}

func newRuntimeCorpusCoverage() *runtimeCorpusCoverage {
	return &runtimeCorpusCoverage{
		assemblyFiles:     make(map[string]bool),
		activeFunctions:   make(map[runtimeFunctionKey]goc.RuntimeCoverageFunction),
		compiledFunctions: make(map[runtimeFunctionKey]bool),
		executedFunctions: make(map[runtimeFunctionKey]bool),
		compiledBlocks:    make(map[runtimeBlockKey]goc.RuntimeCoverageBlock),
		executedBlocks:    make(map[runtimeBlockKey]bool),
	}
}

func (coverage *runtimeCorpusCoverage) add(capability runtimeCapability, result runtimeCapabilityResult) {
	if *runtimeCoverageProfile == "" {
		return
	}
	coverage.programs++
	program := runtimeCoverageProgramReport{
		Category:        capability.category,
		Name:            capability.name,
		Source:          capability.source,
		Expectation:     runtimeCoverageExpectationName(capability.expectation),
		CompileOutcome:  result.compileOutcome,
		RunOutcome:      result.runOutcome,
		CoverageOutcome: result.coverageOutcome,
	}
	if result.err != nil {
		program.Error = result.err.Error()
		program.Output = truncateCoverageFailure(result.output)
	}
	if result.coverageErr != nil {
		program.CoverageError = result.coverageErr.Error()
	}
	coverage.programResults = append(coverage.programResults, program)
	if result.coverage == nil {
		return
	}
	programSourceID := result.coverage.metadata.RuntimeSourceID
	if programSourceID == "" {
		coverage.collectionErrors = append(
			coverage.collectionErrors,
			fmt.Sprintf("%s/%s returned coverage without a runtime source ID", capability.category, capability.name),
		)
		return
	}
	if coverage.runtimeSourceID == "" {
		coverage.runtimeSourceID = programSourceID
	} else if coverage.runtimeSourceID != programSourceID {
		coverage.collectionErrors = append(
			coverage.collectionErrors,
			fmt.Sprintf(
				"%s/%s runtime source ID is %s, want %s",
				capability.category,
				capability.name,
				programSourceID,
				coverage.runtimeSourceID,
			),
		)
		return
	}
	coverage.coveredPrograms++
	for _, assembly := range result.coverage.metadata.AssemblyFiles {
		coverage.assemblyFiles[assembly] = true
	}

	for _, function := range result.coverage.metadata.Functions {
		key := runtimeFunctionKey{name: function.Name, file: function.File, line: function.Line}
		coverage.activeFunctions[key] = function
		if function.Compiled {
			coverage.compiledFunctions[key] = true
		}
		if function.Entry >= 0 && function.Entry < len(result.coverage.hits) && result.coverage.hits[function.Entry] {
			coverage.executedFunctions[key] = true
		}
	}
	for _, block := range result.coverage.metadata.Blocks {
		key := runtimeBlockKey{
			function: block.Function,
			file:     block.File,
			line:     block.Line,
			block:    block.Block,
		}
		coverage.compiledBlocks[key] = block
		if block.ID >= 0 && block.ID < len(result.coverage.hits) && result.coverage.hits[block.ID] {
			coverage.executedBlocks[key] = true
		}
	}
}

func (coverage *runtimeCorpusCoverage) report() runtimeCoverageReport {
	report := runtimeCoverageReport{
		Version:         runtimeCoverageReportVersion,
		GOOS:            "linux",
		GOARCH:          "arm64",
		RuntimeSourceID: coverage.runtimeSourceID,
		Scope:           "build-selected runtime Go source functions and emitted cg12 basic blocks; selected Plan 9 assembly is listed separately",
		Programs:        append([]runtimeCoverageProgramReport(nil), coverage.programResults...),
		Summary: runtimeCoverageSummary{
			Programs:          coverage.programs,
			CoveredPrograms:   coverage.coveredPrograms,
			ActiveFunctions:   len(coverage.activeFunctions),
			CompiledFunctions: len(coverage.compiledFunctions),
			ExecutedFunctions: len(coverage.executedFunctions),
			FunctionCoverage:  coveragePercent(len(coverage.executedFunctions), len(coverage.activeFunctions)),
			CompiledBlocks:    len(coverage.compiledBlocks),
			ExecutedBlocks:    len(coverage.executedBlocks),
			BlockCoverage:     coveragePercent(len(coverage.executedBlocks), len(coverage.compiledBlocks)),
		},
	}
	for assembly := range coverage.assemblyFiles {
		report.AssemblyFiles = append(report.AssemblyFiles, assembly)
	}
	sort.Strings(report.AssemblyFiles)
	sort.Slice(report.Programs, func(left, right int) bool {
		return runtimeCoverageProgramName(report.Programs[left]) < runtimeCoverageProgramName(report.Programs[right])
	})
	for _, program := range report.Programs {
		if program.Error != "" && program.Expectation == "must-pass" {
			report.Summary.UnexpectedFailures++
		}
	}

	files := make(map[string]*runtimeCoverageFileReport)
	fileReport := func(name string) *runtimeCoverageFileReport {
		if files[name] == nil {
			files[name] = &runtimeCoverageFileReport{File: name}
		}
		return files[name]
	}
	for key := range coverage.activeFunctions {
		fileReport(key.file).ActiveFunctions++
		reason := "not executed"
		if coverage.compiledFunctions[key] {
			fileReport(key.file).CompiledFunctions++
		} else {
			reason = "not compiled"
		}
		if coverage.executedFunctions[key] {
			fileReport(key.file).ExecutedFunctions++
			report.ExecutedFunctions = append(report.ExecutedFunctions, runtimeCoverageFunctionDiff{
				Name: key.name,
				File: key.file,
				Line: key.line,
			})
			continue
		}
		report.MissingFunctions = append(report.MissingFunctions, runtimeCoverageMissingFunction{
			Name:   key.name,
			File:   key.file,
			Line:   key.line,
			Reason: reason,
		})
	}
	for key, block := range coverage.compiledBlocks {
		fileReport(key.file).CompiledBlocks++
		if coverage.executedBlocks[key] {
			fileReport(key.file).ExecutedBlocks++
			report.ExecutedBlocks = append(report.ExecutedBlocks, runtimeCoverageBlockReport{
				Function: block.Function,
				File:     block.File,
				Line:     block.Line,
				Block:    block.Block,
			})
		} else {
			report.UncoveredBlocks = append(report.UncoveredBlocks, runtimeCoverageBlockReport{
				Function: block.Function,
				File:     block.File,
				Line:     block.Line,
				Block:    block.Block,
			})
		}
	}
	for _, file := range files {
		report.Files = append(report.Files, *file)
	}
	sort.Slice(report.Files, func(left, right int) bool {
		return report.Files[left].File < report.Files[right].File
	})
	sort.Slice(report.MissingFunctions, func(left, right int) bool {
		leftFunction := report.MissingFunctions[left]
		rightFunction := report.MissingFunctions[right]
		if leftFunction.Reason != rightFunction.Reason {
			return leftFunction.Reason < rightFunction.Reason
		}
		if leftFunction.File != rightFunction.File {
			return leftFunction.File < rightFunction.File
		}
		if leftFunction.Line != rightFunction.Line {
			return leftFunction.Line < rightFunction.Line
		}
		return leftFunction.Name < rightFunction.Name
	})
	sort.Slice(report.ExecutedFunctions, func(left, right int) bool {
		return runtimeCoverageFunctionDiffLess(report.ExecutedFunctions[left], report.ExecutedFunctions[right])
	})
	sort.Slice(report.ExecutedBlocks, func(left, right int) bool {
		return runtimeCoverageBlockReportLess(report.ExecutedBlocks[left], report.ExecutedBlocks[right])
	})
	sort.Slice(report.UncoveredBlocks, func(left, right int) bool {
		return runtimeCoverageBlockReportLess(report.UncoveredBlocks[left], report.UncoveredBlocks[right])
	})
	return report
}

func (coverage *runtimeCorpusCoverage) write(t *testing.T) {
	if *runtimeCoverageProfile == "" {
		return
	}
	if len(coverage.collectionErrors) > 0 {
		for _, collectionError := range coverage.collectionErrors {
			t.Errorf("runtime coverage collection: %s", collectionError)
		}
		return
	}
	report := coverage.report()
	classifications, err := readRuntimeCoverageClassifications(
		*runtimeCoverageClassifications,
		report.GOOS,
		report.GOARCH,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyRuntimeCoverageClassifications(&report, classifications); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(*runtimeCoverageProfile, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf(
		"runtime coverage: %d/%d active functions (%.2f%%), %d/%d compiled blocks (%.2f%%); report %s",
		report.Summary.ExecutedFunctions,
		report.Summary.ActiveFunctions,
		report.Summary.FunctionCoverage,
		report.Summary.ExecutedBlocks,
		report.Summary.CompiledBlocks,
		report.Summary.BlockCoverage,
		*runtimeCoverageProfile,
	)
}

func defaultRuntimeCoverageClassifications() string {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "runtime_coverage_classifications.json"
	}
	return filepath.Join(filepath.Dir(filename), "runtime_coverage_classifications.json")
}

func coveragePercent(covered, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(covered) * 100 / float64(total)
}

func runtimeCoverageExpectationName(expectation runtimeCapabilityExpectation) string {
	switch expectation {
	case runtimeCapabilityKnownGap:
		return "known-gap"
	case runtimeCapabilityExpectedFailure:
		return "expected-failure"
	default:
		return "must-pass"
	}
}

func truncateCoverageFailure(output string) string {
	const limit = 500
	if len(output) <= limit {
		return output
	}
	return output[:limit] + "\n... truncated ..."
}
