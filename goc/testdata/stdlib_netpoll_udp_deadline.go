package main

import (
	"net"
	"time"
)

func main() {
	address, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		panic("ResolveUDPAddr failed")
	}
	connection, err := net.ListenUDP("udp", address)
	if err != nil {
		panic("ListenUDP failed")
	}
	defer connection.Close()

	if err := connection.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		panic("SetReadDeadline failed")
	}
	buffer := make([]byte, 1)
	_, _, err = connection.ReadFromUDP(buffer)
	if err == nil {
		panic("ReadFromUDP unexpectedly succeeded")
	}
	if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		panic("ReadFromUDP did not return timeout error")
	}
}
