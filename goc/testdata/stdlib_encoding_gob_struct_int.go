package main

import (
	"bytes"
	"encoding/gob"
	"os"
)

type gobStructInt struct {
	Count int
}

func main() {
	original := gobStructInt{Count: 42}

	var encoded bytes.Buffer
	encoder := gob.NewEncoder(&encoded)
	if err := encoder.Encode(original); err != nil {
		println("gob encode struct-int failed:", err.Error())
		os.Exit(1)
	}

	var decoded gobStructInt
	decoder := gob.NewDecoder(&encoded)
	if err := decoder.Decode(&decoded); err != nil {
		println("gob decode struct-int failed:", err.Error())
		os.Exit(1)
	}
	if decoded.Count != original.Count {
		println("gob struct-int mismatch:", decoded.Count)
		os.Exit(1)
	}
}
