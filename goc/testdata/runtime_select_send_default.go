package main

func main() {
	channel := make(chan int, 1)

	select {
	case channel <- 17:
	default:
		panic("send should have been ready")
	}

	selectedDefault := false
	select {
	case channel <- 25:
		panic("send should not have been ready")
	default:
		selectedDefault = true
	}

	if !selectedDefault {
		panic("default case was not selected")
	}
	if <-channel != 17 {
		panic("channel retained the wrong value")
	}
}
