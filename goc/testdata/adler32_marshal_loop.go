package main

import (
	"encoding"
	"hash/adler32"
	"io"
)

func main() {
	input := "abcde"
	for iteration := 0; iteration < 32; iteration++ {
		hash := adler32.New()
		secondHash := adler32.New()
		if _, err := io.WriteString(hash, input[:len(input)/2]); err != nil {
			panic(err)
		}
		state, err := hash.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil {
			panic(err)
		}
		appendedState, err := hash.(encoding.BinaryAppender).AppendBinary(make([]byte, 4, 32))
		if err != nil || string(state) != string(appendedState[4:]) {
			panic("appended state mismatch")
		}
		if err := secondHash.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
			panic(err)
		}
		if _, err := io.WriteString(hash, input[len(input)/2:]); err != nil {
			panic(err)
		}
		if _, err := io.WriteString(secondHash, input[len(input)/2:]); err != nil {
			panic(err)
		}
		if hash.Sum32() != secondHash.Sum32() {
			panic("checksum mismatch")
		}
	}
}
