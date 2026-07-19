package main

import "bytes"

func main() {
	var buffer bytes.Buffer
	buffer.WriteByte('a')
	buffer.WriteString("bc")
	buffer.Write([]byte("def"))

	if buffer.Len() != 6 {
		panic("bytes.Buffer has wrong length")
	}
	if buffer.String() != "abcdef" {
		panic("bytes.Buffer has wrong contents")
	}

	value, err := buffer.ReadByte()
	if err != nil || value != 'a' {
		panic("bytes.Buffer ReadByte returned wrong value")
	}
	if buffer.String() != "bcdef" {
		panic("bytes.Buffer retained wrong contents after ReadByte")
	}
}
