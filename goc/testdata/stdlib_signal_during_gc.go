package main

import (
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

func main() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	defer signal.Stop(signals)

	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				values := make([]*int, 1024)
				for index := range values {
					value := index
					values[index] = &value
				}
				runtime.GC()
				runtime.KeepAlive(values)
			}
		}
	}()

	for range 32 {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
			panic(err)
		}
		select {
		case received := <-signals:
			if received != syscall.SIGUSR1 {
				panic("received wrong signal")
			}
		case <-time.After(2 * time.Second):
			panic("timed out waiting for signal during GC")
		}
	}
	close(done)
}
