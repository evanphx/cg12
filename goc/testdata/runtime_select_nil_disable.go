package main

func main() {
	var disabled chan int
	ready := make(chan int, 1)
	ready <- 91

	select {
	case <-disabled:
		panic("nil channel receive should be disabled")
	case value := <-ready:
		if value != 91 {
			panic("ready channel delivered wrong value")
		}
	default:
		panic("ready channel was not selected")
	}

	select {
	case disabled <- 12:
		panic("nil channel send should be disabled")
	default:
	}
}
