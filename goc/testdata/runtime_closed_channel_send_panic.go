package main

func main() {
	values := make(chan int)
	close(values)

	recovered := false
	func() {
		defer func() {
			recovered = recover() != nil
		}()
		values <- 1
	}()

	if !recovered {
		panic("send on closed channel did not panic")
	}
}
