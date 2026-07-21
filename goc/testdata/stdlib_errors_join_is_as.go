package main

import (
	"errors"
	"runtime"
)

type customError struct {
	message string
}

func (err customError) Error() string {
	return err.message
}

func main() {
	for iteration := 0; iteration < 25; iteration++ {
		target := customError{message: "target"}
		joined := errors.Join(errors.New("left"), target, errors.New("right"))
		if !errors.Is(joined, target) {
			panic("errors.Is did not find joined target")
		}

		runtime.GC()

		var extracted customError
		if !errors.As(joined, &extracted) {
			panic("errors.As did not find custom error")
		}
		if extracted.message != "target" {
			panic("errors.As returned wrong custom error")
		}
	}
}
