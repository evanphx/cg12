package main

import (
	"io"
	"os"
	"strings"
)

func makeManualCopy(writer io.Writer, goroutines []func() error) (*os.File, []func() error) {
	reader, pipeWriter, err := os.Pipe()
	if err != nil {
		panic("pipe failed")
	}

	goroutines = append(goroutines, func() error {
		buffer := make([]byte, 32)
		for {
			count, readErr := reader.Read(buffer)
			if count > 0 {
				if _, writeErr := writer.Write(buffer[:count]); writeErr != nil {
					reader.Close()
					return writeErr
				}
			}
			if readErr == io.EOF {
				return reader.Close()
			}
			if readErr != nil {
				reader.Close()
				return readErr
			}
		}
	})

	return pipeWriter, goroutines
}

func main() {
	var builder strings.Builder
	var goroutines []func() error

	writer, goroutines := makeManualCopy(&builder, goroutines)
	if _, err := writer.Write([]byte("cg12 manual copy")); err != nil {
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
	if builder.String() != "cg12 manual copy" {
		panic("copy output mismatch")
	}
}
