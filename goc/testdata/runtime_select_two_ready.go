package main

func main() {
	left := make(chan int, 1)
	right := make(chan int, 1)
	left <- 17
	right <- 25

	total := 0
	for count := 0; count < 2; count++ {
		select {
		case value := <-left:
			total += value
		case value := <-right:
			total += value
		default:
			panic("ready channels were not selected")
		}
	}

	if total != 42 {
		panic("select over two ready channels lost a value")
	}
}
