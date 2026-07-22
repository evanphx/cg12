package main

import (
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer listener.Close()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		panic(err)
	}
	defer client.Close()

	server, err := listener.Accept()
	if err != nil {
		panic(err)
	}
	defer server.Close()

	readDone := make(chan byte, 1)
	go func() {
		buffer := []byte{0}
		if _, err := server.Read(buffer); err != nil {
			panic(err)
		}
		readDone <- buffer[0]
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	defer signal.Stop(signals)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
		panic(err)
	}
	select {
	case received := <-signals:
		if received != syscall.SIGUSR1 {
			panic("received wrong signal")
		}
	case <-time.After(2 * time.Second):
		panic("timed out waiting for signal during netpoll")
	}

	if _, err := client.Write([]byte{42}); err != nil {
		panic(err)
	}
	select {
	case value := <-readDone:
		if value != 42 {
			panic("netpoll read returned wrong value")
		}
	case <-time.After(2 * time.Second):
		panic("netpoll read did not resume")
	}
}
