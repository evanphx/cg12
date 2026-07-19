package main

import (
	"io"
	"strings"
)

func main() {
	reader := io.NewSectionReader(strings.NewReader("section-reader"), 8, 6)
	contents, err := io.ReadAll(reader)
	if err != nil {
		panic("SectionReader ReadAll returned an error")
	}
	if string(contents) != "reader" {
		panic("SectionReader returned wrong contents")
	}

	position, err := reader.Seek(0, io.SeekStart)
	if err != nil || position != 0 {
		panic("SectionReader SeekStart failed")
	}
	buffer := make([]byte, 3)
	read, err := reader.ReadAt(buffer, 1)
	if err != nil || read != 3 {
		panic("SectionReader ReadAt failed")
	}
	if string(buffer) != "ead" {
		panic("SectionReader ReadAt returned wrong contents")
	}
}
