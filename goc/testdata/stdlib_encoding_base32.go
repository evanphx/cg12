package main

import "encoding/base32"

func main() {
	source := []byte("cg12 base32 status")
	encoded := base32.StdEncoding.EncodeToString(source)
	decoded, err := base32.StdEncoding.DecodeString(encoded)
	if err != nil {
		panic("base32 DecodeString failed")
	}
	if string(decoded) != string(source) {
		panic("base32 roundtrip mismatch")
	}
}
