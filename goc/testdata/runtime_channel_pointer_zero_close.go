package main

type channelPointerNode struct {
	value int
}

func main() {
	channel := make(chan *channelPointerNode, 1)
	channel <- &channelPointerNode{value: 42}
	close(channel)

	first, ok := <-channel
	if !ok || first == nil || first.value != 42 {
		panic("closed channel did not deliver buffered pointer")
	}

	second, ok := <-channel
	if ok {
		panic("empty closed channel reported ok")
	}
	if second != nil {
		panic("empty closed pointer channel did not deliver nil")
	}
}
