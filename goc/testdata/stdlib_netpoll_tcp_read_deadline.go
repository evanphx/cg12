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

	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			panic("Accept failed")
		}
		accepted <- connection
	}()

	client, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		panic("DialTimeout failed")
	}
	defer client.Close()

	server := <-accepted
	defer server.Close()

	if err := client.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		panic("SetReadDeadline failed")
	}
	buffer := make([]byte, 1)
	_, err = client.Read(buffer)
	if err == nil {
		panic("Read unexpectedly succeeded")
	}
	if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		panic("Read did not return timeout error")
	}
}
