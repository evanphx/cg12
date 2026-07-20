package main

import (
	"os"
	"time"
)

func main() {
	for iteration := 0; iteration < 20; iteration++ {
		reader, writer, err := os.Pipe()
		if err != nil {
			panic("pipe failed")
		}

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

		time.Sleep(time.Millisecond)
		if err := writer.Close(); err != nil {
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

		reader.Close()
	}
}
