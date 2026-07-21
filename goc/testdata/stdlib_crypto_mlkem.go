package main

import (
	"bytes"
	"crypto/mlkem"
)

func main() {
	seed := make([]byte, mlkem.SeedSize)
	for index := range seed {
		seed[index] = byte(index)
	}

	decapsulationKey, err := mlkem.NewDecapsulationKey768(seed)
	if err != nil {
		panic(err)
	}
	if !bytes.Equal(decapsulationKey.Bytes(), seed) {
		panic("mlkem seed round trip mismatch")
	}

	encodedPublicKey := decapsulationKey.EncapsulationKey().Bytes()
	encapsulationKey, err := mlkem.NewEncapsulationKey768(encodedPublicKey)
	if err != nil {
		panic(err)
	}
	sharedKey, ciphertext := encapsulationKey.Encapsulate()
	decapsulated, err := decapsulationKey.Decapsulate(ciphertext)
	if err != nil {
		panic(err)
	}
	if !bytes.Equal(sharedKey, decapsulated) {
		panic("mlkem shared key mismatch")
	}
}
