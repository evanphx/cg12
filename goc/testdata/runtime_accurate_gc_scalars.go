package main

import "runtime"

func scalarStack(depth int) uintptr {
	smallScalar := uintptr(1456)
	largeScalar := uintptr(0x777700001234)
	zeroScalar := uintptr(0)

	if depth == 0 {
		runtime.GC()
		return smallScalar + (largeScalar & 0xff) + zeroScalar
	}

	return scalarStack(depth-1) + (smallScalar & 1)
}

func main() {
	got := scalarStack(256)
	if got != 1508 {
		panic("accurate GC scalar computation mismatch")
	}
}
