package main

import "encoding/pem"

func main() {
	block := &pem.Block{
		Type: "CG12 STATUS",
		Headers: map[string]string{
			"Package": "encoding/pem",
		},
		Bytes: []byte("payload"),
	}

	encoded := pem.EncodeToMemory(block)
	decoded, rest := pem.Decode(encoded)
	if decoded == nil {
		panic("pem Decode returned nil")
	}
	if len(rest) != 0 {
		panic("pem Decode left trailing data")
	}
	if decoded.Type != block.Type {
		panic("pem type mismatch")
	}
	if decoded.Headers["Package"] != "encoding/pem" {
		panic("pem header mismatch")
	}
	if string(decoded.Bytes) != "payload" {
		panic("pem payload mismatch")
	}
}
