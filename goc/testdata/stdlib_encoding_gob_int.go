package main

import (
	"bytes"
	"encoding/gob"
	"os"
)

func main() {
	var encoded bytes.Buffer
	encoder := gob.NewEncoder(&encoded)
	if err := encoder.Encode(42); err != nil {
		println("gob encode int failed:", err.Error())
		os.Exit(1)
	}

	var decoded int
	decoder := gob.NewDecoder(&encoded)
	if err := decoder.Decode(&decoded); err != nil {
		println("gob decode int failed:", err.Error())
		os.Exit(1)
	}
	if decoded != 42 {
		println("gob int mismatch:", decoded)
		os.Exit(1)
	}
}
