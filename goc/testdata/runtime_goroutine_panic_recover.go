package main

func main() {
	done := make(chan int, 1)
	go func() {
		defer func() {
			if recover() == nil {
				done <- 1
				return
			}
			done <- 42
		}()
		panic("goroutine panic")
	}()

	if value := <-done; value != 42 {
		panic("goroutine panic recover result mismatch")
	}
}
