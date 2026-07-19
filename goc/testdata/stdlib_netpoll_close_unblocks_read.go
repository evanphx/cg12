package main

import (
	"net"
	"strings"
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
	server := <-accepted
	defer server.Close()

	result := make(chan string, 1)
	go func() {
		buffer := make([]byte, 1)
		_, err := client.Read(buffer)
		if err == nil {
			result <- "nil"
			return
		}
		result <- err.Error()
	}()

	if err := client.Close(); err != nil {
		panic("client Close failed")
	}
	message := <-result
	if !strings.Contains(message, "closed") {
		panic("close did not unblock read with closed error")
	}
}
