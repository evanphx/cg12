package main

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
)

func decodeHKDFHex(value string) []byte {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic("hex DecodeString failed")
	}
	return decoded
}

func main() {
	inputKeyMaterial := bytes.Repeat([]byte{0x0b}, 22)
	salt := decodeHKDFHex("000102030405060708090a0b0c")
	info := decodeHKDFHex("f0f1f2f3f4f5f6f7f8f9")
	expectedPseudorandomKey := decodeHKDFHex("077709362c2e32df0ddc3f0dc47bba6390b6c73bb50f9c3122ec844ad7c2b3e5")
	expectedOutput := decodeHKDFHex("3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf" +
		"34007208d5b887185865")

	pseudorandomKey, err := hkdf.Extract(sha256.New, inputKeyMaterial, salt)
	if err != nil {
		panic("hkdf Extract failed")
	}
	if !bytes.Equal(pseudorandomKey, expectedPseudorandomKey) {
		panic("hkdf extracted key mismatch")
	}

	expanded, err := hkdf.Expand(sha256.New, pseudorandomKey, string(info), len(expectedOutput))
	if err != nil {
		panic("hkdf Expand failed")
	}
	if !bytes.Equal(expanded, expectedOutput) {
		panic("hkdf expanded key mismatch")
	}

	derived, err := hkdf.Key(sha256.New, inputKeyMaterial, salt, string(info), len(expectedOutput))
	if err != nil {
		panic("hkdf Key failed")
	}
	if !bytes.Equal(derived, expectedOutput) {
		panic("hkdf derived key mismatch")
	}
}
