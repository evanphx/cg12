package main

//go:noinline
func retainNothing(args ...any) int { return len(args) }

//go:noinline
func leaky() int {
	x := 42
	return retainNothing(&x)
}

func main() { println(leaky()) }
