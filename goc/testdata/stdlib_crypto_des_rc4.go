package main

import (
	"bytes"
	"crypto/des"
	"crypto/rc4"
	"encoding/hex"
)

func decodeHex(value string) []byte {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return decoded
}

func main() {
	key := decodeHex("133457799bbcdff1")
	plaintext := decodeHex("0123456789abcdef")
	wantCiphertext := decodeHex("85e813540f0ab405")

	desCipher, err := des.NewCipher(key)
	if err != nil {
		panic(err)
	}
	ciphertext := make([]byte, des.BlockSize)
	desCipher.Encrypt(ciphertext, plaintext)
	if !bytes.Equal(ciphertext, wantCiphertext) {
		panic("des vector mismatch")
	}
	decrypted := make([]byte, des.BlockSize)
	desCipher.Decrypt(decrypted, ciphertext)
	if !bytes.Equal(decrypted, plaintext) {
		panic("des round trip mismatch")
	}

	rc4Cipher, err := rc4.NewCipher([]byte("Key"))
	if err != nil {
		panic(err)
	}
	rc4Ciphertext := make([]byte, len("Plaintext"))
	rc4Cipher.XORKeyStream(rc4Ciphertext, []byte("Plaintext"))
	wantRC4 := decodeHex("bbf316e8d940af0ad3")
	if !bytes.Equal(rc4Ciphertext, wantRC4) {
		panic("rc4 vector mismatch")
	}
}
