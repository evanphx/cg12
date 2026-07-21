package main

import (
	"io"
	"os"
)

func makeReaderCloser() (*os.File, func() error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		panic("pipe failed")
	}

	closeReader := func() error {
		return reader.Close()
	}

	return writer, closeReader
}

func main() {
	writer, closeReader := makeReaderCloser()
	if err := writer.Close(); err != nil {
		panic("writer close failed")
	}

	done := make(chan error, 1)
	go func(fn func() error) {
		done <- fn()
	}(closeReader)

	if err := <-done; err != nil && err != io.ErrClosedPipe {
		panic("reader close failed")
	}
}
