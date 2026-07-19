package main

import (
	"bufio"
	"bytes"
)

func main() {
	var backing bytes.Buffer
	writer := bufio.NewWriterSize(&backing, 4)
	if _, err := writer.WriteString("buf"); err != nil {
		panic("bufio writer returned an error")
	}
	if backing.Len() != 0 {
		panic("bufio writer flushed too early")
	}
	if _, err := writer.WriteString("io"); err != nil {
		panic("bufio writer second write returned an error")
	}
	if err := writer.Flush(); err != nil {
		panic("bufio Flush returned an error")
	}

	reader := bufio.NewReaderSize(&backing, 2)
	first, err := reader.ReadByte()
	if err != nil || first != 'b' {
		panic("bufio ReadByte returned wrong first byte")
	}
	rest, err := reader.ReadString('o')
	if err != nil {
		panic("bufio ReadString returned an error")
	}
	if rest != "ufio" {
		panic("bufio ReadString returned wrong contents")
	}
}
