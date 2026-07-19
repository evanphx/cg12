package main

func produce(values chan int) {
	for value := 1; value <= 6; value++ {
		values <- value
	}
	close(values)
}

func main() {
	values := make(chan int, 2)
	go produce(values)

	total := 0
	count := 0
	for value := range values {
		total += value
		count++
	}

	if count != 6 || total != 21 {
		panic("channel close/range result mismatch")
	}
}
