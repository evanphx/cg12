package main

import (
	"io"
	"os"
	"strings"
)

func makeInterfaceCaptureCloser(writer io.Writer) (*os.File, func() error) {
	reader, pipeWriter, err := os.Pipe()
	if err != nil {
		panic("pipe failed")
	}

	closeReader := func() error {
		if _, err := writer.Write([]byte("captured")); err != nil {
			return err
		}
		return reader.Close()
	}

	return pipeWriter, closeReader
}

func main() {
	var builder strings.Builder
	writer, closeReader := makeInterfaceCaptureCloser(&builder)
	if err := writer.Close(); err != nil {
		panic("writer close failed")
	}

	done := make(chan error, 1)
	go func(fn func() error) {
		done <- fn()
	}(closeReader)

	if err := <-done; err != nil {
		panic("reader close failed")
	}
	if builder.String() != "captured" {
		panic("interface capture failed")
	}
}
