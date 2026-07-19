package main

func main() {
	channel := make(chan int, 1)

	select {
	case <-channel:
		panic("empty channel receive should block")
	default:
	}

	channel <- 42
	select {
	case value := <-channel:
		if value != 42 {
			panic("received wrong channel value")
		}
	default:
		panic("buffered channel receive should be ready")
	}
}
