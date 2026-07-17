package main

import (
	"bytes"
	"io"
)

func main() {
	reader := bytes.NewReader([]byte("abc"))
	written, err := io.CopyN(io.Discard, reader, 1)
	if written != 1 {
		panic("CopyN returned wrong byte count")
	}
	if err != nil {
		panic("CopyN returned an error")
	}
	if reader.Len() != 2 {
		panic("CopyN did not advance the reader")
	}
}
