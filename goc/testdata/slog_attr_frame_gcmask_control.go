// The control for slog_attr_frame_gcmask.go: slog.Value's exact shape, written
// in the program's own package.
//
// A zero-length function field, a uint64 and an any, wrapped in a struct behind
// a string key, returned by value from a non-inlined constructor and held
// across a collection. It prints 200 under both compilers, so neither the
// zero-length field nor the by-value return is on its own enough to produce the
// bad map -- which is what makes the reduction's use of the real log/slog the
// subject rather than the shape.
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
