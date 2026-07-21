package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"hash"
)

func writeHash(digest hash.Hash, chunks ...string) []byte {
	for _, chunk := range chunks {
		if _, err := digest.Write([]byte(chunk)); err != nil {
			panic("hash Write failed")
		}
	}
	return digest.Sum(nil)
}

func requireHex(actual []byte, expected string) {
	if hex.EncodeToString(actual) != expected {
		panic("digest mismatch")
	}
}

func main() {
	sha256Digest := writeHash(sha256.New(), "a", "b", "c")
	requireHex(sha256Digest, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")

	sha512Digest := writeHash(sha512.New(), "ab", "c")
	requireHex(sha512Digest, "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a"+
		"2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f")

	messageAuthenticationCode := hmac.New(sha256.New, []byte("key"))
	if _, err := messageAuthenticationCode.Write([]byte("The quick brown fox jumps over the lazy dog")); err != nil {
		panic("hmac Write failed")
	}
	requireHex(messageAuthenticationCode.Sum(nil), "f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8")

	wrongCode := hmac.New(sha256.New, []byte("wrong key"))
	if _, err := wrongCode.Write([]byte("The quick brown fox jumps over the lazy dog")); err != nil {
		panic("second hmac Write failed")
	}
	if hmac.Equal(messageAuthenticationCode.Sum(nil), wrongCode.Sum(nil)) {
		panic("hmac accepted a different key")
	}
}
