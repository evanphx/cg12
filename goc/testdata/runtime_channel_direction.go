package main

func sendAll(output chan<- int, values []int) {
	for _, value := range values {
		output <- value
	}
	close(output)
}

func receiveAll(input <-chan int) int {
	total := 0
	for value := range input {
		total += value
	}
	return total
}

func main() {
	values := []int{3, 5, 7, 11, 16}
	channel := make(chan int, 2)

	go sendAll(channel, values)
	total := receiveAll(channel)
	if total != 42 {
		panic("directional channel result mismatch")
	}
}
