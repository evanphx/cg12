package main

import (
	"bytes"
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/hex"
)

func mustDecodeHex(value string) []byte {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return decoded
}

func main() {
	sha3Digest := sha3.Sum256(nil)
	wantSHA3 := mustDecodeHex("a7ffc6f8bf1ed76651c14756a061d662f580ff4de43b49fa82d80a4b80f8434a")
	if !bytes.Equal(sha3Digest[:], wantSHA3) {
		panic("sha3-256 vector mismatch")
	}

	shakeDigest := sha3.SumSHAKE128(nil, 32)
	wantSHAKE := mustDecodeHex("7f9c2ba4e88f827d616045507605853ed73b8093f6efbc88eb1a6eacfa66ef26")
	if !bytes.Equal(shakeDigest, wantSHAKE) {
		panic("shake128 vector mismatch")
	}

	derived, err := pbkdf2.Key(sha256.New, "password", []byte("salt"), 1, 32)
	if err != nil {
		panic(err)
	}
	wantPBKDF2 := mustDecodeHex("120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b")
	if !bytes.Equal(derived, wantPBKDF2) {
		panic("pbkdf2 vector mismatch")
	}
}
