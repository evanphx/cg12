package main

import "encoding/base64"

func main() {
	source := []byte("cg12 status suite")
	encoded := base64.StdEncoding.EncodeToString(source)
	if encoded != "Y2cxMiBzdGF0dXMgc3VpdGU=" {
		panic("base64 encoded wrong string")
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		panic("base64 decode failed")
	}
	if string(decoded) != string(source) {
		panic("base64 decoded wrong bytes")
	}

	raw := base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3, 252})
	if raw != "AQID_A" {
		panic("raw URL base64 encoded wrong string")
	}
}
