package main

import (
	"io"
	"os"
	"strings"
)

type noWriteTo struct{}

func (noWriteTo) WriteTo(io.Writer) (int64, error) {
	panic("hidden WriteTo was called")
}

type fileWithoutWriteTo struct {
	noWriteTo
	*os.File
}

func makeEmbeddedFileCopy(writer io.Writer, goroutines []func() error) (*os.File, []func() error) {
	reader, pipeWriter, err := os.Pipe()
	if err != nil {
		panic("pipe failed")
	}

	goroutines = append(goroutines, func() error {
		_, copyErr := io.Copy(writer, fileWithoutWriteTo{File: reader})
		closeErr := reader.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})

	return pipeWriter, goroutines
}

func main() {
	var builder strings.Builder
	var goroutines []func() error

	writer, goroutines := makeEmbeddedFileCopy(&builder, goroutines)
	if _, err := writer.Write([]byte("cg12 embedded file copy")); err != nil {
		panic("write failed")
	}
	if err := writer.Close(); err != nil {
		panic("writer close failed")
	}

	errc := make(chan error, 1)
	for _, fn := range goroutines {
		go func(fn func() error) {
			errc <- fn()
		}(fn)
	}

	if err := <-errc; err != nil {
		panic("copy goroutine failed")
	}
	if builder.String() != "cg12 embedded file copy" {
		panic("copy output mismatch")
	}
}
