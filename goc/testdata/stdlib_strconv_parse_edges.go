package main

import "strconv"

func main() {
	unsigned, err := strconv.ParseUint("ff", 16, 8)
	if err != nil {
		panic("strconv ParseUint failed")
	}
	if unsigned != 255 {
		panic("strconv ParseUint decoded wrong value")
	}

	signed, err := strconv.ParseInt("-100", 10, 8)
	if err != nil {
		panic("strconv ParseInt failed")
	}
	if signed != -100 {
		panic("strconv ParseInt decoded wrong value")
	}

	if _, err := strconv.ParseInt("128", 10, 8); err == nil {
		panic("strconv ParseInt did not report range error")
	}
}
