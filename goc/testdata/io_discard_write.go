package main

import "io"

func writeThroughFunction(writer io.Writer) {
	n, err := writer.Write([]byte("x"))
	if n != 1 || err != nil {
		panic("writer returned the wrong result")
	}
}

func main() {
	n, err := io.Discard.Write([]byte("x"))
	if n != 1 || err != nil {
		panic("io.Discard returned the wrong result")
	}

	writeThroughFunction(io.Discard)
}
