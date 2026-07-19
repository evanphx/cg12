package main

func main() {
	values := []int{1, 2, 3, 4}
	array := (*[3]int)(values[1:])
	array[0] = 20
	array[2] = 40

	if values[1] != 20 || values[3] != 40 {
		panic("slice-to-array pointer did not alias backing array")
	}

	defer func() {
		if recover() == nil {
			panic("short slice-to-array pointer conversion did not panic")
		}
	}()
	_ = (*[5]int)(values)
}
