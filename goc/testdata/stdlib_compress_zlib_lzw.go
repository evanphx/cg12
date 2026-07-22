package main

import (
	"bytes"
	"compress/lzw"
	"compress/zlib"
	"io"
)

type lzwByteWriter struct {
	destination *bytes.Buffer
}

func (writer lzwByteWriter) Write(data []byte) (int, error) {
	return writer.destination.Write(data)
}

func (writer lzwByteWriter) WriteByte(value byte) error {
	return writer.destination.WriteByte(value)
}

func (lzwByteWriter) Flush() error {
	return nil
}

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

	lowWidthInput := []byte{0, 1, 2, 3, 0, 2, 1, 3, 3, 2, 1, 0}
	var lowWidthBuffer bytes.Buffer
	lowWidthDestination := lzwByteWriter{destination: &lowWidthBuffer}
	lowWidthWriter := lzw.NewWriter(lowWidthDestination, lzw.LSB, 2)
	if _, err := lowWidthWriter.Write(lowWidthInput); err != nil {
		panic("low-width lzw Write failed")
	}
	if err := lowWidthWriter.Close(); err != nil {
		panic("low-width lzw Close failed")
	}
	lowWidthReader := lzw.NewReader(&lowWidthBuffer, lzw.LSB, 2)
	lowWidthOutput, err := io.ReadAll(lowWidthReader)
	if err != nil {
		panic("low-width lzw ReadAll failed")
	}
	if err := lowWidthReader.Close(); err != nil {
		panic("low-width lzw reader Close failed")
	}
	if !bytes.Equal(lowWidthOutput, lowWidthInput) {
		panic("low-width lzw roundtrip mismatch")
	}
}
