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
	defer writer.Close()

	deadline := time.Now().Add(-time.Millisecond)
	if err := reader.SetReadDeadline(deadline); err != nil {
		println("pipe-past-deadline: SetReadDeadline failed")
		println(err.Error())
		panic("SetReadDeadline failed")
	}

	buffer := make([]byte, 1)
	_, err = reader.Read(buffer)
	if err == nil {
		panic("read unexpectedly succeeded")
	}
	if !os.IsTimeout(err) {
		println("pipe-past-deadline: read failed without timeout")
		println(err.Error())
		panic("read failed without timeout")
	}
}
