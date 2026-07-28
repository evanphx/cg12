package goc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStdlibOverlayRejectsAssemblyReplacement(t *testing.T) {
	directory := t.TempDir()
	assemblyPath := filepath.Join(directory, "replacement.s")
	if err := os.WriteFile(assemblyPath, []byte("RET\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	manifest := stdlibOverlayManifest{
		Version: 1,
		Files: []stdlibOverlayEntry{
			{
				Package:        "runtime",
				Path:           "src/runtime/set_vma_name_linux.go",
				File:           "replacement.s",
				Mode:           "replace",
				Reason:         "test invalid assembly replacement",
				UpstreamSHA256: "unused",
			},
		},
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(directory, "manifest.json")
	if err := os.WriteFile(manifestPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(stdlibOverlayEnvironment, manifestPath)

	_, err = loadStdlibOverlay(repositoryStdlibRoot(), HostTarget())
	if err == nil {
		t.Fatal("assembly replacement was accepted")
	}
	if !strings.Contains(err.Error(), "overlays must be Go or native .ssa") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStdlibOverlayRejectsReplacementWithoutUpstreamHash(t *testing.T) {
	directory := t.TempDir()
	replacementPath := filepath.Join(directory, "replacement.go")
	if err := os.WriteFile(replacementPath, []byte("package runtime\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	manifest := stdlibOverlayManifest{
		Version: 1,
		Files: []stdlibOverlayEntry{
			{
				Package: "runtime",
				Path:    "src/runtime/set_vma_name_linux.go",
				File:    "replacement.go",
				Mode:    "replace",
				Reason:  "test missing provenance",
			},
		},
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(directory, "manifest.json")
	if err := os.WriteFile(manifestPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(stdlibOverlayEnvironment, manifestPath)

	_, err = loadStdlibOverlay(repositoryStdlibRoot(), HostTarget())
	if err == nil {
		t.Fatal("replacement without an upstream hash was accepted")
	}
	if !strings.Contains(err.Error(), "has no upstream SHA-256") {
		t.Fatalf("unexpected error: %v", err)
	}
}
