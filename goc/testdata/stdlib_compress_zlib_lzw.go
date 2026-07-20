package main

import (
	"bytes"
	"compress/lzw"
	"compress/zlib"
	"io"
)

func main() {
	input := []byte("cg12 compression status cg12 compression status")

	var zlibBuffer bytes.Buffer
	zlibWriter := zlib.NewWriter(&zlibBuffer)
	if _, err := zlibWriter.Write(input); err != nil {
		panic("zlib Write failed")
	}
	if err := zlibWriter.Close(); err != nil {
		panic("zlib Close failed")
	}
	zlibReader, err := zlib.NewReader(&zlibBuffer)
	if err != nil {
		panic("zlib NewReader failed")
	}
	zlibOutput, err := io.ReadAll(zlibReader)
	if err != nil {
		panic("zlib ReadAll failed")
	}
	if err := zlibReader.Close(); err != nil {
		panic("zlib reader Close failed")
	}
	if !bytes.Equal(zlibOutput, input) {
		panic("zlib roundtrip mismatch")
	}

	var lzwBuffer bytes.Buffer
	lzwWriter := lzw.NewWriter(&lzwBuffer, lzw.LSB, 8)
	if _, err := lzwWriter.Write(input); err != nil {
		panic("lzw Write failed")
	}
	if err := lzwWriter.Close(); err != nil {
		panic("lzw Close failed")
	}
	lzwReader := lzw.NewReader(&lzwBuffer, lzw.LSB, 8)
	lzwOutput, err := io.ReadAll(lzwReader)
	if err != nil {
		panic("lzw ReadAll failed")
	}
	if err := lzwReader.Close(); err != nil {
		panic("lzw reader Close failed")
	}
	if !bytes.Equal(lzwOutput, input) {
		panic("lzw roundtrip mismatch")
	}
}
