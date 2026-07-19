package main

func main() {
	var input chan int
	output := make(chan int)

	selectedDefault := false
	select {
	case <-input:
		panic("nil receive channel was ready")
	case output <- 1:
		panic("nil send alternative should not be bypassed by an unready send")
	default:
		selectedDefault = true
	}

	if !selectedDefault {
		panic("select default did not run")
	}
}
