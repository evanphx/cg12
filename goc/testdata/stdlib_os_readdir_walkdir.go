package main

import (
	"io/fs"
	"os"
	"path/filepath"
)

func main() {
	directory, err := os.MkdirTemp("", "cg12-os-readdir-*")
	if err != nil {
		panic("MkdirTemp failed")
	}
	defer os.RemoveAll(directory)

	subdirectory := filepath.Join(directory, "sub")
	if err := os.Mkdir(subdirectory, 0o755); err != nil {
		panic("Mkdir failed")
	}
	if err := os.WriteFile(filepath.Join(directory, "a.txt"), []byte("a"), 0o644); err != nil {
		panic("WriteFile a failed")
	}
	if err := os.WriteFile(filepath.Join(subdirectory, "b.txt"), []byte("b"), 0o644); err != nil {
		panic("WriteFile b failed")
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		panic("ReadDir failed")
	}
	if len(entries) != 2 {
		panic("ReadDir returned wrong entry count")
	}

	files := 0
	directories := 0
	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories++
		} else {
			files++
		}
		return nil
	})
	if err != nil {
		panic("WalkDir failed")
	}
	if files != 2 || directories != 2 {
		panic("WalkDir returned wrong counts")
	}
}
