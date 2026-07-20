package main

import (
	"os"
	"time"
)

func main() {
	for iteration := 0; iteration < 12; iteration++ {
		reader, writer, err := os.Pipe()
		if err != nil {
			panic("pipe failed")
		}

		if err := reader.SetReadDeadline(time.Now().Add(time.Hour)); err != nil {
			panic("long SetReadDeadline failed")
		}
		if err := reader.SetReadDeadline(time.Now().Add(2 * time.Millisecond)); err != nil {
			panic("short SetReadDeadline failed")
		}

		buffer := make([]byte, 1)
		_, err = reader.Read(buffer)
		if err == nil {
			panic("read unexpectedly succeeded")
		}
		if !os.IsTimeout(err) {
			println("pipe-deadline-reset: read failed without timeout")
			println(err.Error())
			panic("read failed without timeout")
		}

		reader.Close()
		writer.Close()
	}
}
