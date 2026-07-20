package main

import "encoding/hex"

func main() {
	source := []byte{0, 1, 2, 10, 15, 16, 255}
	encoded := hex.EncodeToString(source)
	if encoded != "0001020a0f10ff" {
		panic("hex encoded wrong string")
	}

	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		panic("hex decode failed")
	}
	if len(decoded) != len(source) {
		panic("hex decoded wrong length")
	}
	for index := range source {
		if decoded[index] != source[index] {
			panic("hex decoded wrong byte")
		}
	}
}
