package main

func main() {
	channel := make(chan int, 1)

	selectedDefault := false
	select {
	case <-channel:
		panic("receive should not have been ready")
	default:
		selectedDefault = true
	}
	if !selectedDefault {
		panic("default receive case was not selected")
	}

	channel <- 33
	select {
	case value := <-channel:
		if value != 33 {
			panic("received wrong value")
		}
	default:
		panic("receive should have been ready")
	}
}
