package main

import (
	"bytes"
	"io"
	"strings"
)

func main() {
	var tapped bytes.Buffer
	reader := io.TeeReader(strings.NewReader("tee-reader"), &tapped)

	contents, err := io.ReadAll(reader)
	if err != nil {
		panic("TeeReader returned an error")
	}
	if string(contents) != "tee-reader" {
		panic("TeeReader read wrong contents")
	}
	if tapped.String() != "tee-reader" {
		panic("TeeReader wrote wrong contents")
	}
}
