package main

import (
	"bytes"
	"context"
	"runtime/trace"
)

func main() {
	var buffer bytes.Buffer
	if err := trace.Start(&buffer); err != nil {
		panic("trace Start failed")
	}
	trace.Log(context.Background(), "status", "trace")
	trace.Stop()
	if buffer.Len() == 0 {
		panic("trace log buffer empty")
	}
}
