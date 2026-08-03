// slog.Value's exact shape, written in the program's own package: a
// zero-length function field, a uint64 and an any, wrapped in a struct behind a
// string key, returned by value from a non-inlined constructor and held across
// a collection.
//
// It was kept as the reduction's control because it passes on main. That reads
// as "the shape is not the trigger" and it is not what it means: this program's
// frame map claims the same word the reduction's does, holding the same 200,
// and it survives only because nothing here copies main's stack while the value
// is live. slog_attr_frame_gcmask_shape.go is this program with a stack copy
// instead of a collection, and it fails.
//
// So what this one holds down is narrower than it looks: that a program with a
// bad frame map is not guaranteed to die, and that the mark phase alone will
// walk past a claimed word holding a small integer.
package main

import "runtime"

type value struct {
	_   [0]func()
	num uint64
	any any
}

type attr struct {
	Key   string
	Value value
}

var kind any = 4

//go:noinline
func makeAttr(key string, n uint64) attr {
	return attr{Key: key, Value: value{num: n, any: kind}}
}

//go:noinline
func hold(a attr) uint64 {
	runtime.GC()
	return a.Value.num
}

func main() { println(hold(makeAttr("k", 200))) }
