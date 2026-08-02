package main

import (
	"log/slog"
	"runtime"
)

//go:noinline
func hold(a slog.Attr) int64 {
	runtime.GC()
	return a.Value.Int64()
}

func main() { println(hold(slog.Int("k", 200))) }
