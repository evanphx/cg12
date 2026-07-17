package main

import (
	"bytes"
	"testing"
	"unicode/utf8"
)

var integerSink int
var bytesSink []byte

func main() {
	input := []byte("☺☻☹")
	empty := []byte("")
	replacement := []byte("<>")

	countAllocs := testing.AllocsPerRun(10, func() {
		integerSink = bytes.Count(input, empty)
	})
	println("Count allocations", countAllocs)

	runeCountAllocs := testing.AllocsPerRun(10, func() {
		integerSink = utf8.RuneCount(input)
	})
	println("RuneCount allocations", runeCountAllocs)

	replaceAllocs := testing.AllocsPerRun(10, func() {
		bytesSink = bytes.Replace(input, empty, replacement, -1)
	})
	println("Replace allocations", replaceAllocs)
}
