package main

import (
	"bytes"
	"io"
)

func main() {
	reader := bytes.NewReader([]byte("abc"))
	limited := io.LimitReader(reader, 1)
	buffer := make([]byte, 4)

	n, err := limited.Read(buffer)
	if n != 1 {
		panic("limited reader returned wrong byte count")
	}
	if err != nil {
		panic("limited reader returned an error")
	}
	if reader.Len() != 2 {
		panic("limited reader did not advance the underlying reader")
	}

	n, err = limited.Read(buffer)
	if n != 0 {
		panic("exhausted limited reader returned bytes")
	}
	if err != io.EOF {
		panic("exhausted limited reader did not return EOF")
	}
}
