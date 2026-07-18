package main

import (
	"bufio"
	"bytes"
	"io"
)

func main() {
	reader := bufio.NewReaderSize(bytes.NewReader([]byte("foo")), 16)
	contents, err := io.ReadAll(reader)
	if err != nil {
		panic("ReadAll returned an error")
	}
	if string(contents) != "foo" {
		panic("ReadAll returned the wrong contents")
	}

	line, prefix, err := reader.ReadLine()
	if len(line) != 0 || prefix || err != io.EOF {
		panic("ReadLine did not return EOF")
	}
}
