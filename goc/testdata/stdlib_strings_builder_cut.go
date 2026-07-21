package main

import "strings"

func main() {
	var builder strings.Builder
	builder.Grow(32)
	builder.WriteString("alpha")
	builder.WriteByte('-')
	builder.WriteString("beta")

	text := builder.String()
	before, after, found := strings.Cut(text, "-")
	if !found {
		panic("strings.Cut did not find separator")
	}
	if before != "alpha" || after != "beta" {
		panic("strings.Cut returned wrong parts")
	}
	if !strings.Contains(text, "ha-be") {
		panic("strings.Contains failed")
	}
}
