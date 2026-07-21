package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
)

func decodeEd25519Hex(value string) []byte {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic("hex DecodeString failed")
	}
	return decoded
}

func main() {
	seed := decodeEd25519Hex("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	expectedPublicKey := decodeEd25519Hex("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
	expectedSignature := decodeEd25519Hex("e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e06522490155" +
		"5fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b")

	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		panic("ed25519 public key type mismatch")
	}
	if !bytes.Equal(publicKey, expectedPublicKey) {
		panic("ed25519 public key mismatch: " + hex.EncodeToString(publicKey))
	}

	signature := ed25519.Sign(privateKey, nil)
	if !bytes.Equal(signature, expectedSignature) {
		panic("ed25519 signature mismatch: " + hex.EncodeToString(signature))
	}
	if !ed25519.Verify(publicKey, nil, signature) {
		panic("ed25519 signature verification failed")
	}
	signature[0] ^= 1
	if ed25519.Verify(publicKey, nil, signature) {
		panic("ed25519 accepted a modified signature")
	}
}
