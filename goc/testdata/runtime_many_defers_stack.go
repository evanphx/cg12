package main

func stackWithManyDefers(depth int, total *int) {
	defer func() {
		*total += depth
	}()
	var padding [8]int
	padding[0] = depth
	if depth > 0 {
		stackWithManyDefers(depth-1, total)
	}
	if padding[0] < 0 {
		panic("unreachable")
	}
}

func main() {
	total := 0
	stackWithManyDefers(128, &total)
	if total != 8256 {
		panic("many defers across stack recursion produced wrong total")
	}
}
