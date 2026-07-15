package main

import "fmt"

func main() {
	written, err := fmt.Println("hello", 42)
	if err != nil || written != 9 {
		panic("fmt.Println returned an incorrect result")
	}
}
