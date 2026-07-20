package main

func main() {
	values := []string{"gamma", "alpha", "beta"}
	if !(values[1] < values[2]) {
		panic("string slice compare mismatch")
	}

	values[0], values[1] = values[1], values[0]
	if values[0] != "alpha" {
		panic("string slice swap first mismatch")
	}
	if values[1] != "gamma" {
		panic("string slice swap second mismatch")
	}
}
