package main

import (
	"os"
	"reflect"
)

func main() {
	value := reflect.ValueOf(42)
	if value.Kind() != reflect.Int {
		println("reflect int kind mismatch:", int(value.Kind()))
		os.Exit(1)
	}
	if value.Int() != 42 {
		println("reflect int value mismatch:", value.Int())
		os.Exit(1)
	}
}
