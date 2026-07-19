package main

import (
	"io"
	"os"
)

func main() {
	reader, writer, err := os.Pipe()
	if err != nil {
		panic("pipe failed")
	}
	defer reader.Close()
	defer writer.Close()

	done := make(chan string, 1)
	go func() {
		payload := []byte("netpoll-pipe")
		if _, err := writer.Write(payload); err != nil {
			println("pipe-roundtrip: write failed")
			println(err.Error())
			panic("write failed")
		}
		done <- "write-complete"
	}()

	buffer := make([]byte, len("netpoll-pipe"))
	if _, err := io.ReadFull(reader, buffer); err != nil {
		println("pipe-roundtrip: read failed")
		println(err.Error())
		panic("read failed")
	}
	if string(buffer) != "netpoll-pipe" {
		panic("wrong pipe payload")
	}
	if <-done != "write-complete" {
		panic("writer did not complete")
	}
}
