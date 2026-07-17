package main

import (
	"bytes"
	"testing"
)

func main() {
	x := []byte{'x'}
	y := []byte{'y'}
	temporary := make([]byte, 72)
	startSizes := []int{0, 100, 1000, 10000, 100000}
	growSizes := []int{10000, 100000}
	for _, growLength := range growSizes {
		for _, startLength := range startSizes {
			println("case", startLength, growLength)
			xBytes := bytes.Repeat(x, startLength)
			buffer := bytes.NewBuffer(xBytes)
			readBytes, _ := buffer.Read(temporary)
			yBytes := bytes.Repeat(y, growLength)
			allocations := testing.AllocsPerRun(100, func() {
				buffer.Grow(growLength)
				buffer.Write(yBytes)
			})
			if allocations != 0 {
				println("allocation", startLength, growLength, allocations)
			}
			if !bytes.Equal(buffer.Bytes()[0:startLength-readBytes], xBytes[readBytes:]) {
				println("bad initial data", startLength, growLength)
			}
			if !bytes.Equal(buffer.Bytes()[startLength-readBytes:startLength-readBytes+growLength], yBytes) {
				println("bad written data", startLength, growLength)
			}
		}
	}
	println("done")
}
