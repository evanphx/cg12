package main

import (
	"bytes"
	"log"
	"strings"
)

func main() {
	var buffer bytes.Buffer
	logger := log.New(&buffer, "status: ", 0)
	logger.Printf("%s=%d", "answer", 42)

	line := buffer.String()
	if !strings.HasPrefix(line, "status: answer=42") {
		panic("log prefix mismatch")
	}
	if !strings.HasSuffix(line, "\n") {
		panic("log newline mismatch")
	}
}
