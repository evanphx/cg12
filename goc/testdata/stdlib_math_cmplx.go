package main

import (
	"math"
	"math/cmplx"
)

func closeEnough(got float64, want float64) bool {
	delta := math.Abs(got - want)
	return delta < 0.000000001
}

func main() {
	value := complex(3.0, 4.0)
	if cmplx.Abs(value) != 5 {
		panic("complex abs mismatch")
	}

	sqrt := cmplx.Sqrt(complex(-1.0, 0.0))
	if !closeEnough(real(sqrt), 0) {
		panic("complex sqrt real mismatch")
	}
	if !closeEnough(imag(sqrt), 1) {
		panic("complex sqrt imaginary mismatch")
	}

	polarRadius, polarTheta := cmplx.Polar(complex(0.0, 2.0))
	if !closeEnough(polarRadius, 2) {
		panic("polar radius mismatch")
	}
	if !closeEnough(polarTheta, math.Pi/2) {
		panic("polar theta mismatch")
	}
}
