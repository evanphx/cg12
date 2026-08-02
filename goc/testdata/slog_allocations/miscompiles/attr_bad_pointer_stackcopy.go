package main

import "log/slog"

var sink int

//go:noinline
func grow(depth int) {
	var pad [512]int
	pad[0] = depth
	if depth > 0 {
		grow(depth - 1)
	}
	sink += pad[0]
}

//go:noinline
func hold(a slog.Attr) int64 {
	grow(400)
	return a.Value.Int64()
}

func main() { println(hold(slog.Int("k", 200))) }
