package main

import "io"

func main() {
	reader, writer := io.Pipe()
	done := make(chan string, 1)

	go func() {
		contents, err := io.ReadAll(reader)
		if err != nil {
			panic("Pipe reader returned an error")
		}
		done <- string(contents)
	}()

	written, err := writer.Write([]byte("pipe"))
	if err != nil {
		panic("Pipe writer returned an error")
	}
	if written != 4 {
		panic("Pipe writer returned wrong count")
	}
	if err := writer.Close(); err != nil {
		panic("Pipe close returned an error")
	}

	if <-done != "pipe" {
		panic("Pipe delivered wrong contents")
	}
}
