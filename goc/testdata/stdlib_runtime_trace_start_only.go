package main

import (
	"bytes"
	"runtime/trace"
)

func main() {
	var buffer bytes.Buffer
	if err := trace.Start(&buffer); err != nil {
		panic("trace Start failed")
	}
}
