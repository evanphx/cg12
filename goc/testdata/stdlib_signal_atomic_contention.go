package main

import (
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	var counter uint64
	stop := make(chan struct{})
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					atomic.AddUint64(&counter, 1)
				}
			}
		}()
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	for range 64 {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
			panic(err)
		}
		select {
		case received := <-signals:
			if received != syscall.SIGUSR1 {
				panic("received wrong signal")
			}
		case <-time.After(2 * time.Second):
			panic("timed out waiting for signal during atomic contention")
		}
	}
	signal.Stop(signals)
	close(stop)
	workers.Wait()
	if atomic.LoadUint64(&counter) == 0 {
		panic("atomic workers did not run")
	}
}
