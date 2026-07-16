package main

import "context"

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	done := ctx.Done()
	if done == nil {
		panic("context returned a nil Done channel")
	}
	cancel()
	<-done
}
