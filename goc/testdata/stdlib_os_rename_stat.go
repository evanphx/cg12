package main

import (
	"os"
	"path/filepath"
)

func main() {
	directory, err := os.MkdirTemp("", "cg12-os-rename-stat-*")
	if err != nil {
		panic("MkdirTemp failed")
	}
	defer os.RemoveAll(directory)

	source := filepath.Join(directory, "source.txt")
	destination := filepath.Join(directory, "destination.txt")

	if err := os.WriteFile(source, []byte("rename-stat"), 0o644); err != nil {
		panic("WriteFile failed")
	}
	if err := os.Rename(source, destination); err != nil {
		panic("Rename failed")
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		panic("source still exists after rename")
	}
	info, err := os.Stat(destination)
	if err != nil {
		panic("Stat destination failed")
	}
	if info.Name() != "destination.txt" || info.Size() != int64(len("rename-stat")) {
		panic("Stat returned wrong file info")
	}
}
