package main

type channelStructPayload struct {
	value int
	next  *channelStructPayload
}

func main() {
	channel := make(chan channelStructPayload, 1)
	channel <- channelStructPayload{value: 7, next: &channelStructPayload{value: 8}}
	close(channel)

	first, ok := <-channel
	if !ok {
		panic("buffered value was not received from closed channel")
	}
	if first.value+first.next.value != 15 {
		panic("closed channel delivered corrupted struct value")
	}

	second, ok := <-channel
	if ok {
		panic("empty closed channel reported ok")
	}
	if second.value != 0 || second.next != nil {
		panic("empty closed channel did not deliver zero struct")
	}
}
