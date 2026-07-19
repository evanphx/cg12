package main

func main() {
	channel := make(chan int, 1)
	select {
	case channel <- 17:
	default:
		panic("buffered select send was not ready")
	}

	select {
	case value := <-channel:
		if value != 17 {
			panic("buffered select receive got wrong value")
		}
	default:
		panic("buffered select receive was not ready")
	}

	select {
	case channel <- 23:
		select {
		case value := <-channel:
			if value != 23 {
				panic("nested select receive got wrong value")
			}
		default:
			panic("nested receive was not ready")
		}
	default:
		panic("nested send was not ready")
	}
}
