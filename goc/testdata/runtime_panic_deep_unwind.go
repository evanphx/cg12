package main

func recurseForPanic(depth int) {
	var padding [16]int
	padding[0] = depth
	if depth == 0 {
		panic("deep panic")
	}
	recurseForPanic(depth - 1)
	if padding[0] == -1 {
		panic("unreachable")
	}
}

func main() {
	defer func() {
		value := recover()
		if value != "deep panic" {
			panic("wrong panic value recovered after deep unwind")
		}
	}()
	recurseForPanic(256)
}
