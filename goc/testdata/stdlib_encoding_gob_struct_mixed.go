package main

import (
	"bytes"
	"encoding/gob"
	"os"
)

type gobStructMixed struct {
	Name   string
	Count  int
	Values []int
}

func main() {
	original := gobStructMixed{
		Name:   "cg12",
		Count:  42,
		Values: []int{3, 5, 8, 13},
	}

	var encoded bytes.Buffer
	encoder := gob.NewEncoder(&encoded)
	if err := encoder.Encode(original); err != nil {
		println("gob encode mixed struct failed:", err.Error())
		os.Exit(1)
	}

	var decoded gobStructMixed
	decoder := gob.NewDecoder(&encoded)
	if err := decoder.Decode(&decoded); err != nil {
		println("gob decode mixed struct failed:", err.Error())
		os.Exit(1)
	}
	if decoded.Name != original.Name {
		println("gob mixed string mismatch:", decoded.Name)
		os.Exit(1)
	}
	if decoded.Count != original.Count {
		println("gob mixed int mismatch:", decoded.Count)
		os.Exit(1)
	}
	if len(decoded.Values) != len(original.Values) || decoded.Values[2] != original.Values[2] {
		println("gob mixed slice mismatch")
		os.Exit(1)
	}
}
