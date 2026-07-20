package main

import "net"

func main() {
	address, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		panic("ResolveUDPAddr failed")
	}
	connection, err := net.ListenUDP("udp", address)
	if err != nil {
		println("udp-burst: ListenUDP failed")
		println(err.Error())
		panic("ListenUDP failed")
	}
	defer connection.Close()

	localAddress := connection.LocalAddr().(*net.UDPAddr)
	for index := 0; index < 32; index++ {
		payload := []byte{byte(index), byte(index ^ 0x55)}
		if _, err := connection.WriteToUDP(payload, localAddress); err != nil {
			panic("WriteToUDP failed")
		}
	}

	total := 0
	buffer := make([]byte, 8)
	for index := 0; index < 32; index++ {
		read, remote, err := connection.ReadFromUDP(buffer)
		if err != nil {
			panic("ReadFromUDP failed")
		}
		if read != 2 {
			panic("wrong datagram length")
		}
		if remote == nil || remote.Port == 0 {
			panic("invalid remote address")
		}
		total += int(buffer[0])
	}
	if total != (31 * 32 / 2) {
		panic("wrong datagram total")
	}
}
