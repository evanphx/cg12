package main

func growBytes(bytes []byte, count int) []byte {
	defer func() {
		if recover() != nil {
			panic("grow failed")
		}
	}()

	capacity := len(bytes) + count
	grown := append([]byte(nil), make([]byte, capacity)...)
	length := copy(grown, bytes)
	return grown[:length]
}

func main() {
	bytes := make([]byte, 1024)
	bytes[0] = 42
	grown := growBytes(bytes, 1024)
	if len(grown) != 1024 || cap(grown) < 2048 || grown[0] != 42 {
		panic("wrong grown slice")
	}
	println("append-make ok")
}
