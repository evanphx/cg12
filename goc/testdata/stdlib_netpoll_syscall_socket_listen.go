package main

import "syscall"

func main() {
	fileDescriptor, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		println("syscall-socket-listen: socket failed")
		errno, ok := err.(syscall.Errno)
		if ok {
			println("syscall-socket-listen: socket errno", uintptr(errno))
		} else {
			println("syscall-socket-listen: socket error is not errno")
		}
		panic("socket failed")
	}
	defer syscall.Close(fileDescriptor)

	address := &syscall.SockaddrInet4{
		Port: 0,
		Addr: [4]byte{127, 0, 0, 1},
	}
	if err := syscall.Bind(fileDescriptor, address); err != nil {
		println("syscall-socket-listen: bind failed")
		panic("bind failed")
	}
	if err := syscall.Listen(fileDescriptor, 1); err != nil {
		println("syscall-socket-listen: listen failed")
		panic("listen failed")
	}

	socketAddress, err := syscall.Getsockname(fileDescriptor)
	if err != nil {
		println("syscall-socket-listen: getsockname failed")
		panic("getsockname failed")
	}
	listenAddress, ok := socketAddress.(*syscall.SockaddrInet4)
	if !ok {
		println("syscall-socket-listen: getsockname returned wrong address type")
		panic("getsockname returned wrong address type")
	}
	if listenAddress.Port == 0 {
		println("syscall-socket-listen: kernel did not assign a port")
		panic("kernel did not assign a port")
	}
	if listenAddress.Addr != [4]byte{127, 0, 0, 1} {
		println("syscall-socket-listen: kernel returned wrong listen address")
		panic("kernel returned wrong listen address")
	}
}
