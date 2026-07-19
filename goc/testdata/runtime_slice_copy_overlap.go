package main

func main() {
	values := []int{1, 2, 3, 4, 5, 6}
	copied := copy(values[2:], values[:4])
	if copied != 4 {
		panic("copy returned the wrong count")
	}

	expected := []int{1, 2, 1, 2, 3, 4}
	for index, value := range expected {
		if values[index] != value {
			panic("overlapping copy result mismatch")
		}
	}

	more := append(values[:0], values...)
	if len(more) != 6 || cap(more) != cap(values) {
		panic("append into existing backing array changed shape")
	}
}
