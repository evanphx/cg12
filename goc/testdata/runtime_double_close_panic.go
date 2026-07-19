package main

func main() {
	values := make(chan int)
	close(values)

	recovered := false
	func() {
		defer func() {
			recovered = recover() != nil
		}()
		close(values)
	}()

	if !recovered {
		panic("double close did not panic")
	}
}
