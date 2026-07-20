package main

import (
	"bytes"
	"compress/gzip"
	"io"
)

func main() {
	input := []byte("cg12 go compiler gzip roundtrip\ncg12 go compiler gzip roundtrip\n")
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(input); err != nil {
		panic("gzip Write failed")
	}
	if err := writer.Close(); err != nil {
		panic("gzip Close failed")
	}

	reader, err := gzip.NewReader(&compressed)
	if err != nil {
		panic("gzip NewReader failed")
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		panic("gzip ReadAll failed")
	}
	if err := reader.Close(); err != nil {
		panic("gzip reader Close failed")
	}
	if !bytes.Equal(output, input) {
		panic("gzip roundtrip mismatch")
	}
}
