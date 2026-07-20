package main

import (
	"io"
	"net"
	"time"
)

func main() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		println("tcp-churn: Listen failed")
		println(err.Error())
		panic("Listen failed")
	}
	defer listener.Close()

	address := listener.Addr().String()
	for index := 0; index < 24; index++ {
		done := make(chan string, 1)
		go serveTCPStressConnection(listener, byte(index), done)

		connection, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			println("tcp-churn: DialTimeout failed")
			println(err.Error())
			panic("DialTimeout failed")
		}
		payload := []byte{byte(index), byte(index + 1)}
		if _, err := connection.Write(payload); err != nil {
			panic("Write failed")
		}
		buffer := make([]byte, 2)
		if _, err := io.ReadFull(connection, buffer); err != nil {
			panic("ReadFull failed")
		}
		if buffer[0] != payload[1] || buffer[1] != payload[0] {
			panic("wrong echo payload")
		}
		connection.Close()

		select {
		case result := <-done:
			if result != "server-done" {
				panic(result)
			}
		case <-time.After(time.Second):
			panic("server did not complete")
		}
	}
}

func serveTCPStressConnection(listener net.Listener, index byte, done chan<- string) {
	connection, err := listener.Accept()
	if err != nil {
		println("tcp-churn: Accept failed")
		println(err.Error())
		done <- "Accept failed"
		return
	}
	if connection == nil {
		done <- "Accept returned nil connection"
		return
	}
	defer connection.Close()

	buffer := make([]byte, 2)
	if _, err := io.ReadFull(connection, buffer); err != nil {
		println("tcp-churn: server ReadFull failed")
		println(err.Error())
		done <- "ReadFull failed"
		return
	}
	if buffer[0] != index || buffer[1] != index+1 {
		done <- "server received wrong payload"
		return
	}
	response := []byte{buffer[1], buffer[0]}
	if _, err := connection.Write(response); err != nil {
		println("tcp-churn: server Write failed")
		println(err.Error())
		done <- "Write failed"
		return
	}
	done <- "server-done"
}
