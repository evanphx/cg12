package main

import "math/bits"

func main() {
	high, low := bits.Mul64(^uint64(0), ^uint64(0))
	if high != 0xfffffffffffffffe || low != 1 {
		panic("wide Mul64 mismatch")
	}

	sum, carry := bits.Add64(^uint64(0), 1, 0)
	if sum != 0 || carry != 1 {
		panic("Add64 carry mismatch")
	}

	difference, borrow := bits.Sub64(0, 1, 0)
	if difference != ^uint64(0) || borrow != 1 {
		panic("Sub64 borrow mismatch")
	}
}
