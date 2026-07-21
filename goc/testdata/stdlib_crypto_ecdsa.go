package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
)

func main() {
	curve := elliptic.P256()
	privateKey, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		panic(err)
	}

	message := []byte("cg12 ecdsa status")
	digest := sha256.Sum256(message)
	signature, err := ecdsa.SignASN1(rand.Reader, privateKey, digest[:])
	if err != nil {
		panic(err)
	}
	if !ecdsa.VerifyASN1(&privateKey.PublicKey, digest[:], signature) {
		panic("ecdsa signature did not verify")
	}

	signature[len(signature)-1] ^= 1
	if ecdsa.VerifyASN1(&privateKey.PublicKey, digest[:], signature) {
		panic("ecdsa accepted a modified signature")
	}
}
