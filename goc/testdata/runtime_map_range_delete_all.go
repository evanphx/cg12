package main

func main() {
	values := make(map[int]int)
	for index := 0; index < 128; index++ {
		values[index] = index * 3
	}

	seen := 0
	for key := range values {
		seen++
		delete(values, key)
	}

	if seen != 128 {
		panic("range did not visit original map entries")
	}
	if len(values) != 0 {
		panic("range delete did not remove all map entries")
	}
}
