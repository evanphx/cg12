package main

func main() {
	channel := make(chan int, 4)
	channel <- 3
	channel <- 5
	channel <- 11
	channel <- 23

	total := 0
	for index := 0; index < 4; index++ {
		total = total*10 + <-channel
	}
	if total != 3633 {
		panic("buffered channel FIFO order mismatch")
	}
}
