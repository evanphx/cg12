package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
)

func decodeX509Hex(value string) []byte {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic("hex DecodeString failed")
	}
	return decoded
}

func main() {
	seed := decodeX509Hex("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)

	publicEncoding, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		panic("x509 public key marshal failed: " + err.Error())
	}
	parsedPublicValue, err := x509.ParsePKIXPublicKey(publicEncoding)
	if err != nil {
		panic("x509 public key parse failed: " + err.Error())
	}
	parsedPublic, ok := parsedPublicValue.(ed25519.PublicKey)
	if !ok || !bytes.Equal(parsedPublic, publicKey) {
		panic("x509 public key mismatch")
	}

	privateEncoding, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		panic("x509 private key marshal failed: " + err.Error())
	}
	parsedPrivateValue, err := x509.ParsePKCS8PrivateKey(privateEncoding)
	if err != nil {
		panic("x509 private key parse failed: " + err.Error())
	}
	parsedPrivate, ok := parsedPrivateValue.(ed25519.PrivateKey)
	if !ok || !bytes.Equal(parsedPrivate, privateKey) {
		panic("x509 private key mismatch")
	}

	message := []byte("cg12 x509 status")
	signature := ed25519.Sign(parsedPrivate, message)
	if !ed25519.Verify(parsedPublic, message, signature) {
		panic("x509 parsed keys failed signature round trip")
	}
}
