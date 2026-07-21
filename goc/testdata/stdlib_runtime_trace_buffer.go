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
	ctx := context.Background()
	trace.WithRegion(ctx, "status-region", func() {
		trace.Log(ctx, "status", "trace")
	})
	trace.Stop()
	if buffer.Len() == 0 {
		panic("trace buffer empty")
	}
}
