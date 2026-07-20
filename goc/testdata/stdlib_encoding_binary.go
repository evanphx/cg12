package main

import (
	"bytes"
	"encoding/binary"
)

type binaryRecord struct {
	Count uint16
	Flags uint16
	Total uint32
}

func main() {
	record := binaryRecord{
		Count: 7,
		Flags: 0x1234,
		Total: 0xabcdef01,
	}

	var buffer bytes.Buffer
	if err := binary.Write(&buffer, binary.LittleEndian, record); err != nil {
		panic("binary Write failed")
	}

	var decoded binaryRecord
	if err := binary.Read(&buffer, binary.LittleEndian, &decoded); err != nil {
		panic("binary Read failed")
	}

	if decoded != record {
		panic("binary roundtrip mismatch")
	}
}
