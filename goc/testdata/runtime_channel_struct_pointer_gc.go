package main

import "runtime"

type channelPayloadNode struct {
	value int
	next  *channelPayloadNode
}

type channelPayload struct {
	head *channelPayloadNode
	mark int
}

func main() {
	channel := make(chan channelPayload, 1)
	head := &channelPayloadNode{
		value: 17,
		next:  &channelPayloadNode{value: 25},
	}
	channel <- channelPayload{head: head, mark: 99}
	head = nil

	runtime.GC()
	payload := <-channel
	if payload.mark != 99 || payload.head.value+payload.head.next.value != 42 {
		panic("channel payload pointer graph was corrupted")
	}
}
