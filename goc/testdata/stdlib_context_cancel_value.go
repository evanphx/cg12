package main

import "context"

type contextKey string

func main() {
	parent := context.WithValue(context.Background(), contextKey("answer"), 42)
	ctx, cancel := context.WithCancel(parent)

	if got := ctx.Value(contextKey("answer")).(int); got != 42 {
		panic("context value mismatch")
	}

	select {
	case <-ctx.Done():
		panic("context canceled too early")
	default:
	}

	cancel()

	select {
	case <-ctx.Done():
	default:
		panic("context did not cancel")
	}

	if ctx.Err() != context.Canceled {
		panic("context cancel error mismatch")
	}
}
