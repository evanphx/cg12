package main

import (
	"bytes"
	"os"
	"runtime/trace"
)

func main() {
	var buffer bytes.Buffer
	println("before trace start")
	if err := trace.Start(&buffer); err != nil {
		println("trace Start failed:", err.Error())
		os.Exit(1)
	}
	println("after trace start")
	os.Exit(0)
}
