package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

func waitForSignal(signals <-chan os.Signal, want os.Signal) {
	select {
	case received := <-signals:
		if received != want {
			panic("received wrong signal")
		}
	case <-time.After(2 * time.Second):
		panic("timed out waiting for signal")
	}
}

func sendSignal(value syscall.Signal) {
	if err := syscall.Kill(syscall.Getpid(), value); err != nil {
		panic(err)
	}
}

func main() {
	first := make(chan os.Signal, 1)
	signal.Notify(first, syscall.SIGUSR1)
	for range 8 {
		sendSignal(syscall.SIGUSR1)
		waitForSignal(first, syscall.SIGUSR1)
	}

	signal.Stop(first)
	signal.Ignore(syscall.SIGUSR1)
	sendSignal(syscall.SIGUSR1)
	select {
	case <-first:
		panic("stopped channel received a signal")
	case <-time.After(20 * time.Millisecond):
	}

	signal.Reset(syscall.SIGUSR1)
	second := make(chan os.Signal, 1)
	signal.Notify(second, syscall.SIGUSR1)
	sendSignal(syscall.SIGUSR1)
	waitForSignal(second, syscall.SIGUSR1)
	signal.Stop(second)
}
