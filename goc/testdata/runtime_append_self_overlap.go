package main

func main() {
	values := []int{1, 2, 3, 4}
	values = append(values[:1], values[2:]...)
	if len(values) != 3 {
		panic("self append produced wrong length")
	}
	if values[0] != 1 || values[1] != 3 || values[2] != 4 {
		panic("self append produced wrong contents")
	}

	bytes := []byte("abcdef")
	bytes = append(bytes[:2], bytes[4:]...)
	if string(bytes) != "abef" {
		panic("self append byte overlap failed")
	}
}
