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

	timer := time.AfterFunc(20*time.Millisecond, func() {
		if err := writer.Close(); err != nil {
			println("pipe-afterfunc-close: writer close failed")
			println(err.Error())
			panic("writer close failed")
		}
	})
	defer timer.Stop()

	buffer := make([]byte, 1)
	_, err = reader.Read(buffer)
	if err == nil {
		panic("read unexpectedly succeeded")
	}
}
