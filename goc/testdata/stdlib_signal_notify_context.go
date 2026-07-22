package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		panic(err)
	}
	select {
	case received := <-signals:
		if received != syscall.SIGUSR1 {
			panic("received wrong signal")
		}
	case <-time.After(2 * time.Second):
		panic("timed out waiting for signal notification")
	}
	signal.Stop(signals)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGUSR2)
	defer stop()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR2); err != nil {
		panic(err)
	}
	select {
	case <-ctx.Done():
		if ctx.Err() != context.Canceled {
			panic("signal context cancellation mismatch")
		}
	case <-time.After(2 * time.Second):
		panic("timed out waiting for signal context")
	}
}
