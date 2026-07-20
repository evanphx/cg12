package main

import (
	"os"
	"path/filepath"
)

func main() {
	directory, err := os.MkdirTemp("", "cg12-filepath-glob-*")
	if err != nil {
		panic("MkdirTemp failed")
	}
	defer os.RemoveAll(directory)

	if err := os.WriteFile(filepath.Join(directory, "alpha.txt"), []byte("a"), 0o644); err != nil {
		panic("WriteFile alpha failed")
	}
	if err := os.WriteFile(filepath.Join(directory, "beta.log"), []byte("b"), 0o644); err != nil {
		panic("WriteFile beta failed")
	}
	if err := os.Mkdir(filepath.Join(directory, "nested"), 0o755); err != nil {
		panic("Mkdir nested failed")
	}
	if err := os.WriteFile(filepath.Join(directory, "nested", "gamma.txt"), []byte("g"), 0o644); err != nil {
		panic("WriteFile gamma failed")
	}

	matches, err := filepath.Glob(filepath.Join(directory, "*.txt"))
	if err != nil {
		panic("Glob failed")
	}
	if len(matches) != 1 || filepath.Base(matches[0]) != "alpha.txt" {
		panic("Glob returned wrong matches")
	}

	cleaned := filepath.Clean(filepath.Join(directory, "nested", "..", "nested", "gamma.txt"))
	if filepath.Base(cleaned) != "gamma.txt" || filepath.Dir(cleaned) != filepath.Join(directory, "nested") {
		panic("Clean returned wrong path")
	}
}
