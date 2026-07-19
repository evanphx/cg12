package main

func main() {
	buffer := []byte{'x', 'x', 'x', 'x', 'x'}
	count := copy(buffer[1:], "go")
	if count != 2 {
		panic("copy from string returned wrong count")
	}
	if string(buffer) != "xgoxx" {
		panic("copy from string wrote wrong bytes")
	}
}
