package main

func main() {
	left := complex(3.0, 4.0)
	right := complex(2.0, -1.0)
	product := left * right

	if real(product) != 10.0 {
		panic("complex real result mismatch")
	}
	if imag(product) != 5.0 {
		panic("complex imaginary result mismatch")
	}
}
