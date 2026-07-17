package main

func makeBuffer() any {
	buffer := new([]byte)
	*buffer = make([]byte, 8192)
	return buffer
}

func main() {
	value := makeBuffer()
	buffer := value.(*[]byte)
	if len(*buffer) != 8192 {
		panic("interface contained the wrong slice length")
	}
}
