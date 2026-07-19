package main

func main() {
	values := make(map[int]int)
	for index := 0; index < 50; index++ {
		values[index] = index
	}

	for key := range values {
		if key < 10 {
			values[100+key] = 100 + key
		}
	}

	total := 0
	for _, value := range values {
		total += value
	}
	if total != 2270 {
		panic("map range insert produced wrong contents")
	}
}
