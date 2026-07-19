package main

func compute() (result int) {
	result = 17
	defer func() {
		result += 25
	}()
	return result
}

func main() {
	if compute() != 42 {
		panic("defer did not update named return")
	}
}
