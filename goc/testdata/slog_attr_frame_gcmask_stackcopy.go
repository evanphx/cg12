// The same defect as slog_attr_frame_gcmask.go through the other walker.
//
// The collection is replaced by a recursion deep enough to make the runtime
// copy the stack, so the frame is walked by runtime.adjustframe rather than by
// runtime.scanframeworker. Both halves of the runtime that read a frame's
// pointer map are here, which is what says a rejection is the map being wrong
// rather than one walker being wrong.
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
