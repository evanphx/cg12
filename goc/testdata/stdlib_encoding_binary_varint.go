package main

import "encoding/binary"

func main() {
	buffer := make([]byte, binary.MaxVarintLen64)
	count := binary.PutVarint(buffer, -123456789)
	value, read := binary.Varint(buffer[:count])
	if read != count {
		panic("binary Varint consumed wrong byte count")
	}
	if value != -123456789 {
		panic("binary Varint decoded wrong value")
	}

	count = binary.PutUvarint(buffer, 987654321)
	unsigned, read := binary.Uvarint(buffer[:count])
	if read != count {
		panic("binary Uvarint consumed wrong byte count")
	}
	if unsigned != 987654321 {
		panic("binary Uvarint decoded wrong value")
	}
}
