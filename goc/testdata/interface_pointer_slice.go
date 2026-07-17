package main

func makeBuffer() any {
	buffer := make([]byte, 8192)
	return &buffer
}

func main() {
	value := makeBuffer()
	buffer := value.(*[]byte)
	if len(*buffer) != 8192 {
		panic("interface contained the wrong slice length")
	}
	(*buffer)[0] = 42
	if (*buffer)[0] != 42 {
		panic("interface contained the wrong slice data")
	}
}
