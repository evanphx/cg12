// Every kind log/slog packs into slog.Value's num word, live in one frame
// across a collection.
//
// slog_attr_frame_gcmask.go is the reduction and holds a single Int64
// attribute; this one holds an Int64, a Bool, a Duration and a Float64 at once,
// because num carries all four and a pointer map that claims that word claims
// it for every one of them. The values are chosen so each prints as a small
// integer: 200, true, three seconds in nanoseconds, and 1.5 doubled.
package main

import (
	"log/slog"
	"runtime"
	"time"
)

//go:noinline
func hold(i, b, d, f slog.Attr) {
	runtime.GC()
	println(i.Value.Int64(), b.Value.Bool(), d.Value.Duration().Nanoseconds(), int64(f.Value.Float64()*2))
}

func main() {
	hold(
		slog.Int("i", 200),
		slog.Bool("b", true),
		slog.Duration("d", 3*time.Second),
		slog.Float64("f", 1.5),
	)
}
