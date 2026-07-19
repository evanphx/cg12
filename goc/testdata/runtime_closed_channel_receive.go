package main

func main() {
	values := make(chan int, 1)
	values <- 42
	close(values)

	first, firstOK := <-values
	second, secondOK := <-values

	if first != 42 || !firstOK {
		panic("buffered closed receive lost the queued value")
	}
	if second != 0 || secondOK {
		panic("closed receive did not return the zero value with ok=false")
	}
}
