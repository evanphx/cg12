package main

import (
	"math"
	"math/bits"
)

func main() {
	hypot := math.Hypot(3, 4)
	if hypot != 5 {
		panic("hypot mismatch")
	}

	rounded := math.Round(math.Pi * 1000)
	if rounded != 3142 {
		panic("round mismatch")
	}

	value := uint64(0x00ff00ff00ff00ff)
	if bits.OnesCount64(value) != 32 {
		panic("ones count mismatch")
	}
	if bits.RotateLeft64(1, 8) != 256 {
		panic("rotate mismatch")
	}
	high, low := bits.Mul64(0xffffffff, 0xffffffff)
	if high != 0 || low != 0xfffffffe00000001 {
		panic("mul64 mismatch")
	}
}
