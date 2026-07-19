package main

import "runtime"

type faninJob struct {
	value int
	next  *faninJob
}

func worker(input <-chan *faninJob, output chan<- int) {
	for job := range input {
		runtime.GC()
		total := 0
		for node := job; node != nil; node = node.next {
			total += node.value
		}
		output <- total
	}
}

func main() {
	input := make(chan *faninJob)
	output := make(chan int, 8)

	for index := 0; index < 4; index++ {
		go worker(input, output)
	}

	for index := 0; index < 8; index++ {
		head := &faninJob{value: index}
		head = &faninJob{value: index + 1, next: head}
		head = &faninJob{value: index + 2, next: head}
		input <- head
	}
	close(input)

	total := 0
	for index := 0; index < 8; index++ {
		total += <-output
	}
	if total != 108 {
		panic("fanin total mismatch")
	}
}
