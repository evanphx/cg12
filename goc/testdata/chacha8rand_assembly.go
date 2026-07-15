package main

import (
	"internal/chacha8rand"
)

func main() {
	seed := [4]uint64{
		0x4847464544434241,
		0x504f4e4d4c4b4a49,
		0x5857565554535251,
		0x3635343332315a59,
	}
	want := [...]uint64{
		0xb773b6063d4616a5, 0x1160af22a66abc3c, 0x8c2599d9418d287c, 0x7ee07e037edc5cd6,
		0xcfaa9ee02d1c16ad, 0x0e090eef8febea79, 0x3c82d271128b5b3e, 0x9c5addc11252a34f,
		0xdf79bb617d6ceea6, 0x36d553591f9d736a, 0xeef0d14e181ee01f, 0x089bfc760ae58436,
		0xd9e52b59cc2ad268, 0xeb2fb4444b1b8aba, 0x4f95c8a692c46661, 0xc3c6323217cae62c,
		0x91ebb4367f4e2e7e, 0x784cf2c6a0ec9bc6, 0x5c34ec5c34eabe20, 0x4f0a8f515570daa8,
		0xfc35dcb4113d6bf2, 0x5b0da44c645554bc, 0x6d963da3db21d9e1, 0xeeaefc3150e500f3,
		0x2d37923dda3750a5, 0x380d7a626d4bc8b0, 0xeeaf68ede3d7ee49, 0xf4356695883b717c,
		0x846a9021392495a4, 0x8e8510549630a61b, 0x18dc02545dbae493, 0x0f8f9ff0a65a3d43,
	}

	var state chacha8rand.State
	state.Init64(seed)
	for _, expected := range want {
		got, ok := state.Next()
		if !ok {
			panic("ChaCha8 state ran out of values")
		}
		if got != expected {
			panic("ChaCha8 assembly returned an incorrect value")
		}
	}
	if _, ok := state.Next(); ok {
		panic("ChaCha8 state returned too many values")
	}
}
