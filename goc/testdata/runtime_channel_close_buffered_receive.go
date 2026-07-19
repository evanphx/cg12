package main

func main() {
	channel := make(chan string, 2)
	channel <- "first"
	channel <- "second"
	close(channel)

	first, ok := <-channel
	if !ok || first != "first" {
		panic("closed buffered channel lost first value")
	}
	second, ok := <-channel
	if !ok || second != "second" {
		panic("closed buffered channel lost second value")
	}
	zero, ok := <-channel
	if ok || zero != "" {
		panic("closed empty channel did not return zero value")
	}
}
