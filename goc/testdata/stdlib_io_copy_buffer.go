package main

import (
	"bytes"
	"io"
)

func main() {
	input := bytes.NewBufferString("copy-buffer")
	var output bytes.Buffer
	buffer := make([]byte, 3)

	written, err := io.CopyBuffer(&output, input, buffer)
	if err != nil {
		panic("CopyBuffer returned an error")
	}
	if written != 11 {
		panic("CopyBuffer returned wrong byte count")
	}
	if output.String() != "copy-buffer" {
		panic("CopyBuffer wrote wrong contents")
	}
}
