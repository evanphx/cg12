package main

const ringSize = 12
const ringRounds = 20

func main() {
	channels := make([]chan int, ringSize+1)
	for index := 0; index < len(channels); index++ {
		channels[index] = make(chan int)
	}

	done := make(chan string, ringSize)
	for index := 0; index < ringSize; index++ {
		input := channels[index]
		output := channels[index+1]
		go runRingWorker(input, output, done)
	}

	for round := 0; round < ringRounds; round++ {
		channels[0] <- round
		result := <-channels[ringSize]
		if result != round+ringSize {
			panic("ring handoff produced wrong result")
		}
	}

	for index := 0; index < ringSize; index++ {
		if <-done != "worker-done" {
			panic("ring worker did not finish")
		}
	}
}

func runRingWorker(input <-chan int, output chan<- int, done chan<- string) {
	for round := 0; round < ringRounds; round++ {
		value := <-input
		output <- value + 1
	}
	done <- "worker-done"
}
