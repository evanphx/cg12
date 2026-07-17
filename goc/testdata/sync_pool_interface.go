package main

import (
	"runtime"
	"sync"
)

var globalPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, 8192)
		return &buffer
	},
}

func makeBuffer() any {
	buffer := make([]byte, 8192)
	return &buffer
}

func main() {
	direct := makeBuffer()
	directBuffer, ok := direct.(*[]byte)
	if !ok {
		panic("direct interface result has the wrong dynamic type")
	}
	if len(*directBuffer) != 8192 {
		panic("direct interface result has the wrong slice length")
	}

	constructor := func() any {
		buffer := make([]byte, 8192)
		return &buffer
	}
	indirect := constructor()
	indirectBuffer, ok := indirect.(*[]byte)
	if !ok {
		panic("function value interface result has the wrong dynamic type")
	}
	if len(*indirectBuffer) != 8192 {
		panic("function value interface result has the wrong slice length")
	}

	pool := sync.Pool{New: constructor}
	pooled := pool.Get()
	pooledBuffer, ok := pooled.(*[]byte)
	if !ok {
		panic("pool interface result has the wrong dynamic type")
	}
	if len(*pooledBuffer) != 8192 {
		panic("local pool interface result has the wrong slice length")
	}

	global := globalPool.Get()
	globalBuffer, ok := global.(*[]byte)
	if !ok {
		panic("global pool interface result has the wrong dynamic type")
	}
	if len(*globalBuffer) != 8192 {
		panic("global pool returned the wrong buffer length")
	}

	globalPool.Put(global)
	runtime.GC()
	afterGC := globalPool.Get()
	afterGCBuffer, ok := afterGC.(*[]byte)
	if !ok {
		panic("global pool interface result after GC has the wrong dynamic type")
	}
	if len(*afterGCBuffer) != 8192 {
		panic("global pool returned the wrong buffer length after GC")
	}
}
