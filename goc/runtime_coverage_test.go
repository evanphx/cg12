package goc

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/ir"
)

func TestExtractRuntimeCoverage(t *testing.T) {
	coverage := &RuntimeCoverage{
		Blocks: []RuntimeCoverageBlock{{ID: 0}, {ID: 1}, {ID: 2}},
	}
	packet := append([]byte("program output"), runtimeCoverageHeader...)
	packet = append(packet, 0, 1, 1)
	packet = append(packet, runtimeCoverageFooter...)

	result, err := ExtractRuntimeCoverage(packet, coverage)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Output, []byte("program output")) {
		t.Fatalf("clean output = %q", result.Output)
	}
	want := []bool{false, true, true}
	for index := range want {
		if result.Hits[index] != want[index] {
			t.Fatalf("hit %d = %t, want %t", index, result.Hits[index], want[index])
		}
	}
}

func TestRuntimeCoverageUsesSelectedRuntimeFilesAndChunkedCounters(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		t.Skip("runtime coverage targets linux/arm64")
	}

	module, coverage, err := CompileExecutableWithRuntimeCoverage("main.go", []byte("package main\nfunc main() {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(coverage.Functions) == 0 || len(coverage.Blocks) <= runtimeCoverageChunkSize {
		t.Fatalf("coverage has %d functions and %d blocks", len(coverage.Functions), len(coverage.Blocks))
	}
	if len(coverage.RuntimeSourceID) != 64 {
		t.Fatalf("runtime source ID has length %d, want 64", len(coverage.RuntimeSourceID))
	}
	for _, function := range coverage.Functions {
		if len(function.File) < len("runtime/") || function.File[:len("runtime/")] != "runtime/" {
			t.Fatalf("function %s uses non-runtime file %q", function.Name, function.File)
		}
	}

	data := make(map[string]*ir.Data)
	for _, datum := range module.Data {
		data[datum.Name] = datum
	}
	wantChunks := (len(coverage.Blocks) + runtimeCoverageChunkSize - 1) / runtimeCoverageChunkSize
	for chunk := 0; chunk < wantChunks; chunk++ {
		if data[runtimeCoverageChunkName(chunk)] == nil {
			t.Fatalf("coverage chunk %d is missing", chunk)
		}
	}
}
