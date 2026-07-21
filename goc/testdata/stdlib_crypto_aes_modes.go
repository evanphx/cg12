package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
)

func decodeHex(value string) []byte {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic("hex DecodeString failed")
	}
	return decoded
}

func main() {
	key := decodeHex("000102030405060708090a0b0c0d0e0f")
	block, err := aes.NewCipher(key)
	if err != nil {
		panic("aes NewCipher failed")
	}

	plaintext := decodeHex("00112233445566778899aabbccddeeff")
	ciphertext := make([]byte, aes.BlockSize)
	block.Encrypt(ciphertext, plaintext)
	if hex.EncodeToString(ciphertext) != "69c4e0d86a7b0430d8cdb78070b4c55a" {
		panic("aes block encryption mismatch")
	}
	decrypted := make([]byte, aes.BlockSize)
	block.Decrypt(decrypted, ciphertext)
	if !bytes.Equal(decrypted, plaintext) {
		panic("aes block decryption mismatch")
	}

	initializationVector := decodeHex("101112131415161718191a1b1c1d1e1f")
	cbcPlaintext := []byte("0123456789abcdefsecond AES block")
	cbcCiphertext := make([]byte, len(cbcPlaintext))
	cipher.NewCBCEncrypter(block, initializationVector).CryptBlocks(cbcCiphertext, cbcPlaintext)
	cbcDecrypted := make([]byte, len(cbcPlaintext))
	cipher.NewCBCDecrypter(block, initializationVector).CryptBlocks(cbcDecrypted, cbcCiphertext)
	if !bytes.Equal(cbcDecrypted, cbcPlaintext) {
		panic("aes CBC round trip mismatch")
	}

	ctrPlaintext := []byte("CTR mode handles a final partial block")
	ctrCiphertext := make([]byte, len(ctrPlaintext))
	cipher.NewCTR(block, initializationVector).XORKeyStream(ctrCiphertext, ctrPlaintext)
	ctrDecrypted := make([]byte, len(ctrPlaintext))
	cipher.NewCTR(block, initializationVector).XORKeyStream(ctrDecrypted, ctrCiphertext)
	if !bytes.Equal(ctrDecrypted, ctrPlaintext) {
		panic("aes CTR round trip mismatch")
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic("aes GCM construction failed")
	}
	nonce := make([]byte, aead.NonceSize())
	additionalData := []byte("cg12 status")
	sealed := aead.Seal(nil, nonce, ctrPlaintext, additionalData)
	opened, err := aead.Open(nil, nonce, sealed, additionalData)
	if err != nil || !bytes.Equal(opened, ctrPlaintext) {
		panic("aes GCM round trip mismatch")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := aead.Open(nil, nonce, sealed, additionalData); err == nil {
		panic("aes GCM accepted modified ciphertext")
	}
}
