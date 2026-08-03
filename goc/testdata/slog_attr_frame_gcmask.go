// A slog.Attr held in a frame across a collection.
//
// log/slog packs int64, uint64, bool, time.Duration and float64 into
// slog.Value's uint64 num field precisely so that those kinds never become
// heap-boxed interfaces. That makes slog.Value a struct whose first word is a
// scalar and whose remaining two are an any, and slog.Attr a struct whose
// pointer words are the key's data pointer and that any -- word 2, holding num,
// is not one of them.
//
// If the frame's pointer map claims word 2 anyway, the collector walks 200 as
// an address and the program dies before it prints:
//
//	runtime: bad pointer in frame main_main at 0x...: 0xc8
//	fatal error: invalid pointer found on stack
//
// The attribute is passed to a non-inlined function so it is live in a frame
// across the collection, and the collection is forced so the failure does not
// depend on allocation load.
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
