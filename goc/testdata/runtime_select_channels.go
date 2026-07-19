package main

func send(value int, target chan int) {
	target <- value
}

func main() {
	left := make(chan int, 1)
	right := make(chan int, 1)

	go send(17, left)
	go send(25, right)

	total := 0
	for total != 42 {
		select {
		case value := <-left:
			total += value
		case value := <-right:
			total += value
		}
	}

	if total != 42 {
		panic("select result mismatch")
	}
}
