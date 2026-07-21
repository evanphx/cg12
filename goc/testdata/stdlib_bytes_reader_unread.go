package main

import (
	"bytes"
	"io"
)

func main() {
	reader := bytes.NewReader([]byte("abc"))
	first, err := reader.ReadByte()
	if err != nil || first != 'a' {
		panic("bytes.Reader ReadByte failed")
	}
	if err := reader.UnreadByte(); err != nil {
		panic("bytes.Reader UnreadByte failed")
	}

	buffer := make([]byte, 3)
	read, err := reader.Read(buffer)
	if err != nil && err != io.EOF {
		panic("bytes.Reader Read failed")
	}
	if read != 3 || string(buffer) != "abc" {
		panic("bytes.Reader unread/read mismatch")
	}
}
