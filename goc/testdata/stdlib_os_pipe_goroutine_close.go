package main

import (
	"io"
	"os"
)

func closeFile(file *os.File, done chan error) {
	done <- file.Close()
}

func main() {
	reader, writer, err := os.Pipe()
	if err != nil {
		panic("pipe failed")
	}

	done := make(chan error, 2)
	go closeFile(writer, done)
	if err := <-done; err != nil {
		panic("writer close failed")
	}

	buffer := make([]byte, 8)
	count, err := reader.Read(buffer)
	if count != 0 {
		panic("read count mismatch")
	}
	if err != io.EOF {
		panic("read did not observe eof")
	}

	go closeFile(reader, done)
	if err := <-done; err != nil {
		panic("reader close failed")
	}
}
