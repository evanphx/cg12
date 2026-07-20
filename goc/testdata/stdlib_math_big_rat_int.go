package main

import "math/big"

func main() {
	left := new(big.Int).SetUint64(1_000_000_000)
	right := new(big.Int).SetUint64(3_000_000_000)
	sum := new(big.Int).Add(left, right)
	if sum.String() != "4000000000" {
		panic("big.Int Add mismatch")
	}

	numerator := big.NewRat(22, 7)
	denominator := big.NewRat(355, 113)
	delta := new(big.Rat).Sub(denominator, numerator)
	if delta.Sign() >= 0 {
		panic("big.Rat Sub sign mismatch")
	}
	if delta.Num().String() != "-1" {
		panic("big.Rat numerator mismatch")
	}
	if delta.Denom().String() != "791" {
		panic("big.Rat denominator mismatch")
	}
}
