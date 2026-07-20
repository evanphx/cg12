package main

import (
	"bytes"
	"io"
	"mime/quotedprintable"
)

func main() {
	var encoded bytes.Buffer
	writer := quotedprintable.NewWriter(&encoded)
	input := []byte("hello=world with trailing spaces   ")
	if _, err := writer.Write(input); err != nil {
		panic("quotedprintable Write failed")
	}
	if err := writer.Close(); err != nil {
		panic("quotedprintable Close failed")
	}

	decoded, err := io.ReadAll(quotedprintable.NewReader(&encoded))
	if err != nil {
		panic("quotedprintable ReadAll failed")
	}
	if !bytes.Equal(decoded, input) {
		panic("quotedprintable roundtrip mismatch")
	}
}
