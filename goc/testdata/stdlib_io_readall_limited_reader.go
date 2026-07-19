package main

import (
	"bytes"
	"io"
)

func main() {
	reader := &io.LimitedReader{
		R: bytes.NewReader([]byte("abcdef")),
		N: 4,
	}
	contents, err := io.ReadAll(reader)
	if err != nil {
		panic("ReadAll returned an error")
	}
	if string(contents) != "abcd" {
		panic("ReadAll returned the wrong limited contents")
	}
	if reader.N != 0 {
		panic("LimitedReader did not consume its limit")
	}
}
