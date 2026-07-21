package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
)

func main() {
	if !crypto.SHA256.Available() {
		panic("sha256 was not registered")
	}
	if crypto.SHA256.Size() != sha256.Size {
		panic("crypto reports the wrong sha256 digest size")
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		panic(err)
	}
	if err := privateKey.Validate(); err != nil {
		panic(err)
	}

	message := []byte("cg12 rsa status")
	digest := sha256.Sum256(message)
	digestSlice := digest[:]
	if len(digestSlice) != sha256.Size {
		panic("sha256 digest slice has the wrong length")
	}
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digestSlice)
	if err != nil {
		panic(err)
	}
	if err := rsa.VerifyPKCS1v15(&privateKey.PublicKey, crypto.SHA256, digestSlice, signature); err != nil {
		panic(err)
	}

	digest[0] ^= 1
	if err := rsa.VerifyPKCS1v15(&privateKey.PublicKey, crypto.SHA256, digestSlice, signature); err == nil {
		panic("rsa accepted a modified digest")
	}
}
