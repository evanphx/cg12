package main

import (
	"net"
	"time"
)

func main() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("Listen failed")
	}
	defer listener.Close()

	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		panic("SetDeadline failed")
	}
	_, err = listener.Accept()
	if err == nil {
		panic("Accept unexpectedly succeeded")
	}
	if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		panic("Accept did not return timeout error")
	}
}
