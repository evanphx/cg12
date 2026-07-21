package main

import (
	"encoding/asn1"
	"math/big"
	"time"
)

type asn1Status struct {
	Version int `asn1:"explicit,tag:0"`
	Serial  *big.Int
	Name    string `asn1:"utf8"`
	Ready   bool
	Created time.Time `asn1:"generalized"`
	Flags   asn1.BitString
	Extra   []byte `asn1:"optional,tag:1"`
}

func main() {
	original := asn1Status{
		Version: 3,
		Serial:  new(big.Int).SetUint64(0x123456789abcdef0),
		Name:    "cg12 compiler",
		Ready:   true,
		Created: time.Date(2026, time.July, 21, 12, 34, 56, 0, time.UTC),
		Flags: asn1.BitString{
			Bytes:     []byte{0xa0},
			BitLength: 3,
		},
		Extra: []byte{1, 2, 3, 5, 8},
	}

	encoded, err := asn1.Marshal(original)
	if err != nil {
		panic("asn1 Marshal failed: " + err.Error())
	}

	var decoded asn1Status
	rest, err := asn1.Unmarshal(encoded, &decoded)
	if err != nil {
		panic("asn1 Unmarshal failed: " + err.Error())
	}
	if len(rest) != 0 {
		panic("asn1 left trailing data")
	}
	if decoded.Version != original.Version || decoded.Serial.Cmp(original.Serial) != 0 {
		panic("asn1 integer field mismatch")
	}
	if decoded.Name != original.Name || !decoded.Ready {
		panic("asn1 scalar field mismatch")
	}
	if !decoded.Created.Equal(original.Created) {
		panic("asn1 time field mismatch")
	}
	if decoded.Flags.BitLength != 3 || decoded.Flags.At(0) != 1 || decoded.Flags.At(1) != 0 {
		panic("asn1 bit string mismatch")
	}
	if len(decoded.Extra) != 5 || decoded.Extra[4] != 8 {
		panic("asn1 optional field mismatch")
	}
}
