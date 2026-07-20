package main

import (
	"io"
	"net"
	"time"
)

func main() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		println("tcp-echo: Listen failed")
		println(err.Error())
		panic("Listen failed")
	}
	defer listener.Close()

	done := make(chan string, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			println("tcp-echo: Accept failed")
			println(err.Error())
			panic("Accept failed")
		}
		defer connection.Close()

		buffer := make([]byte, 4)
		if _, err := io.ReadFull(connection, buffer); err != nil {
			println("tcp-echo: server ReadFull failed")
			println(err.Error())
			panic("server ReadFull failed")
		}
		if string(buffer) != "ping" {
			println("tcp-echo: server received wrong payload")
			panic("server received wrong payload")
		}
		if _, err := connection.Write([]byte("pong")); err != nil {
			println("tcp-echo: server Write failed")
			println(err.Error())
			panic("server Write failed")
		}
		done <- "server-done"
	}()

	client, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		println("tcp-echo: DialTimeout failed")
		println(err.Error())
		panic("DialTimeout failed")
	}
	defer client.Close()

	if _, err := client.Write([]byte("ping")); err != nil {
		println("tcp-echo: client Write failed")
		println(err.Error())
		panic("client Write failed")
	}
	buffer := make([]byte, 4)
	if _, err := io.ReadFull(client, buffer); err != nil {
		println("tcp-echo: client ReadFull failed")
		println(err.Error())
		panic("client ReadFull failed")
	}
	if string(buffer) != "pong" {
		println("tcp-echo: client received wrong payload")
		panic("client received wrong payload")
	}
	if <-done != "server-done" {
		panic("server did not complete")
	}
}
