package main

import (
	"os"
	"time"
)

func main() {
	reader, writer, err := os.Pipe()
	if err != nil {
		panic("pipe failed")
	}
	defer reader.Close()

	done := make(chan string, 1)
	go func() {
		buffer := make([]byte, 1)
		_, err := reader.Read(buffer)
		if err == nil {
			done <- "read-succeeded"
			return
		}
		done <- "read-failed"
	}()

	time.Sleep(10 * time.Millisecond)
	if err := writer.Close(); err != nil {
		println("pipe-close-unblocks-read: writer close failed")
		println(err.Error())
		panic("writer close failed")
	}

	select {
	case result := <-done:
		if result != "read-failed" {
			panic("read unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		panic("read did not unblock")
	}
}
