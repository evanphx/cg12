package main

import "encoding/ascii85"

func main() {
	source := []byte("cg12 ascii85 status")
	encoded := make([]byte, ascii85.MaxEncodedLen(len(source)))
	encodedLength := ascii85.Encode(encoded, source)

	decoded := make([]byte, len(source)+4)
	decodedLength, _, err := ascii85.Decode(decoded, encoded[:encodedLength], true)
	if err != nil {
		panic("ascii85 Decode failed")
	}
	if string(decoded[:decodedLength]) != string(source) {
		panic("ascii85 roundtrip mismatch")
	}
}
