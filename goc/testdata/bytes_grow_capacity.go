package main

import "bytes"

func main() {
	buffer := bytes.NewBuffer(nil)
	contents := bytes.Repeat([]byte{'x'}, 10000)
	for iteration := 0; iteration < 12; iteration++ {
		println("before", iteration, buffer.Len(), buffer.Cap())
		buffer.Grow(len(contents))
		println("grown", iteration, buffer.Len(), buffer.Cap())
		buffer.Write(contents)
		println("written", iteration, buffer.Len(), buffer.Cap())
	}
}
