package main

import (
	"bytes"
	"io"
	"strings"
)

func main() {
	reader := io.MultiReader(
		strings.NewReader("multi"),
		strings.NewReader("-"),
		strings.NewReader("reader"),
	)
	var left bytes.Buffer
	var right bytes.Buffer
	writer := io.MultiWriter(&left, &right)

	written, err := io.Copy(writer, reader)
	if err != nil {
		panic("Copy through MultiReader/MultiWriter returned an error")
	}
	if written != 12 {
		panic("Copy through MultiReader/MultiWriter returned wrong count")
	}
	if left.String() != "multi-reader" || right.String() != "multi-reader" {
		panic("MultiWriter wrote wrong contents")
	}
}
