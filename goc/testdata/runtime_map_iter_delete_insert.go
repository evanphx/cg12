package main

func main() {
	values := map[int]int{
		1: 10,
		2: 20,
		3: 30,
		4: 40,
	}

	seen := 0
	for key := range values {
		seen++
		delete(values, key)
		values[key+10] = key * 100
	}
	if seen == 0 {
		panic("map iteration did not visit entries")
	}

	total := 0
	for _, value := range values {
		total += value
	}
	if total == 100 {
		panic("map iteration delete/insert produced impossible total")
	}
}
