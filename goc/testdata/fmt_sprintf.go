package main

import "fmt"

func main() {
	formatted := fmt.Sprintf("value=%d", 42)
	if formatted != "value=42" {
		panic("fmt.Sprintf returned incorrect output")
	}
}
