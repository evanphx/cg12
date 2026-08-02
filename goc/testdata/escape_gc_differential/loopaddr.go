package main

func main() {
	var pointers []*int
	for index := 0; index < 4; index++ {
		pointers = append(pointers, &index)
	}
	if pointers[0] == pointers[1] {
		println("SHARED")
	} else {
		println("DISTINCT")
	}
	println(*pointers[0], *pointers[1], *pointers[2], *pointers[3])
}
