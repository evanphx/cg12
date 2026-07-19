package main

func main() {
	values := make(chan int)
	close(values)

	selected := false
	select {
	case value, ok := <-values:
		if value != 0 || ok {
			panic("closed select receive returned the wrong value")
		}
		selected = true
	default:
		panic("closed channel was not ready in select")
	}

	if !selected {
		panic("select did not take the closed-channel case")
	}
}
