package main

func pong(input chan int, output chan int) {
	for count := 0; count < 64; count++ {
		value := <-input
		output <- value + 1
	}
}

func main() {
	left := make(chan int)
	right := make(chan int)
	go pong(left, right)

	value := 0
	for count := 0; count < 64; count++ {
		left <- value
		value = <-right
	}

	if value != 64 {
		panic("unbuffered channel ping-pong result mismatch")
	}
}
