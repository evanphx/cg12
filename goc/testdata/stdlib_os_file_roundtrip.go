package main

import (
	"os"
	"path/filepath"
)

func main() {
	directory, err := os.MkdirTemp("", "cg12-os-file-roundtrip-*")
	if err != nil {
		panic("MkdirTemp failed")
	}
	defer os.RemoveAll(directory)

	path := filepath.Join(directory, "value.txt")
	file, err := os.Create(path)
	if err != nil {
		panic("Create failed")
	}
	if _, err := file.WriteString("hello filesystem"); err != nil {
		panic("WriteString failed")
	}
	if _, err := file.Seek(6, 0); err != nil {
		panic("Seek failed")
	}
	buffer := make([]byte, 10)
	read, err := file.Read(buffer)
	if err != nil {
		panic("Read failed")
	}
	if string(buffer[:read]) != "filesystem" {
		panic("Read returned wrong contents")
	}
	if err := file.Close(); err != nil {
		panic("Close failed")
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		panic("ReadFile failed")
	}
	if string(contents) != "hello filesystem" {
		panic("ReadFile returned wrong contents")
	}
}
