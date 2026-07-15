package main

import "io"

type buffer struct {
	data [32]byte
	size int
}

func (b *buffer) Write(data []byte) (int, error) {
	for _, value := range data {
		b.data[b.size] = value
		b.size++
	}
	return len(data), nil
}

func main() {
	var output buffer
	written, err := io.WriteString(&output, "hello, io")
	if err != nil || written != 9 || output.size != 9 {
		panic("io.WriteString failed")
	}
	if output.data[0] != 'h' || output.data[8] != 'o' {
		panic("io.WriteString wrote incorrect data")
	}
}
