package main

func worker(done chan int) {
	defer func() {
		defer func() {
			done <- 42
		}()
	}()
}

func main() {
	done := make(chan int, 1)
	go worker(done)
	if value := <-done; value != 42 {
		panic("unexpected value")
	}
}
