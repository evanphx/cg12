package main

import (
	"io"
	"net"
	"time"
)

func main() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("Listen failed")
	}
	defer listener.Close()

	const clients = 4
	done := make(chan int, clients)
	go func() {
		for count := 0; count < clients; count++ {
			connection, err := listener.Accept()
			if err != nil {
				panic("Accept failed")
			}
			go func(connection net.Conn) {
				defer connection.Close()
				buffer := make([]byte, 1)
				if _, err := io.ReadFull(connection, buffer); err != nil {
					panic("server ReadFull failed")
				}
				if _, err := connection.Write([]byte{buffer[0] + 1}); err != nil {
					panic("server Write failed")
				}
			}(connection)
		}
	}()

	for index := 0; index < clients; index++ {
		go func(value int) {
			connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err != nil {
				panic("DialTimeout failed")
			}
			defer connection.Close()

			if _, err := connection.Write([]byte{byte(value)}); err != nil {
				panic("client Write failed")
			}
			buffer := make([]byte, 1)
			if _, err := io.ReadFull(connection, buffer); err != nil {
				panic("client ReadFull failed")
			}
			done <- int(buffer[0])
		}(index)
	}

	total := 0
	for index := 0; index < clients; index++ {
		total += <-done
	}
	if total != 10 {
		panic("concurrent TCP clients returned wrong total")
	}
}
