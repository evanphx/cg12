package main

import (
	"bytes"
	"crypto/hpke"
)

func main() {
	kem := hpke.MLKEM768()
	privateKey, err := kem.GenerateKey()
	if err != nil {
		panic(err)
	}
	publicKey, err := kem.NewPublicKey(privateKey.PublicKey().Bytes())
	if err != nil {
		panic(err)
	}

	info := []byte("cg12 hpke status")
	plaintext := []byte("authenticated plaintext")
	ciphertext, err := hpke.Seal(publicKey, hpke.HKDFSHA256(), hpke.AES128GCM(), info, plaintext)
	if err != nil {
		panic(err)
	}
	opened, err := hpke.Open(privateKey, hpke.HKDFSHA256(), hpke.AES128GCM(), info, ciphertext)
	if err != nil {
		panic(err)
	}
	if !bytes.Equal(opened, plaintext) {
		panic("hpke plaintext mismatch")
	}

	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := hpke.Open(privateKey, hpke.HKDFSHA256(), hpke.AES128GCM(), info, ciphertext); err == nil {
		panic("hpke accepted modified ciphertext")
	}
}
