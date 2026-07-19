package main

func worker(value int, done chan int) {
	done <- value + 2
}

func main() {
	done := make(chan int, 1)
	go worker(40, done)

	got := <-done
	if got != 42 {
		panic("goroutine channel result mismatch")
	}
}
