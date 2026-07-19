package main

func main() {
	counts := map[rune]int{}
	for _, value := range "gophér gopher" {
		counts[value]++
	}

	total := 0
	for key, count := range counts {
		if key != ' ' {
			total += count
		}
	}

	if total != 12 {
		panic("rune range/map count mismatch")
	}
	if counts['é'] != 1 {
		panic("utf-8 rune decoding mismatch")
	}
}
