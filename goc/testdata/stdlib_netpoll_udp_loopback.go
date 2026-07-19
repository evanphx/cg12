package main

import "net"

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

	payload := []byte("udp")
	if _, err := connection.WriteToUDP(payload, connection.LocalAddr().(*net.UDPAddr)); err != nil {
		panic("WriteToUDP failed")
	}

	buffer := make([]byte, 8)
	read, remote, err := connection.ReadFromUDP(buffer)
	if err != nil {
		panic("ReadFromUDP failed")
	}
	if read != 3 || string(buffer[:read]) != "udp" {
		panic("ReadFromUDP returned wrong payload")
	}
	if remote == nil || remote.Port == 0 {
		panic("ReadFromUDP returned invalid remote address")
	}
}
