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
