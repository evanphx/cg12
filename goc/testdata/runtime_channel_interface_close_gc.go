package main

import "runtime"

type channelInterfaceBox struct {
	value int
	next  *channelInterfaceBox
}

func main() {
	channel := make(chan interface{}, 3)
	channel <- 5
	channel <- &channelInterfaceBox{value: 10, next: &channelInterfaceBox{value: 1}}
	channel <- &channelInterfaceBox{value: 20, next: &channelInterfaceBox{value: 2}}
	close(channel)

	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
	}

	total := 0
	for value := range channel {
		switch typed := value.(type) {
		case int:
			total += typed
		case *channelInterfaceBox:
			total += typed.value
			total += typed.next.value
		default:
			panic("unexpected channel interface value")
		}
	}
	if total != 38 {
		panic("closed channel interface values were corrupted")
	}
}
