package main

import (
	"io"
	"strings"
)

func main() {
	reader := strings.NewReader("seekable")
	position, err := reader.Seek(4, io.SeekStart)
	if err != nil || position != 4 {
		panic("strings.Reader SeekStart failed")
	}

	buffer := make([]byte, 3)
	read, err := reader.Read(buffer)
	if err != nil || read != 3 {
		panic("strings.Reader Read failed after seek")
	}
	if string(buffer) != "abl" {
		panic("strings.Reader read wrong contents after seek")
	}

	position, err = reader.Seek(-2, io.SeekCurrent)
	if err != nil || position != 5 {
		panic("strings.Reader SeekCurrent failed")
	}
	value, err := reader.ReadByte()
	if err != nil || value != 'b' {
		panic("strings.Reader ReadByte returned wrong value")
	}
}
