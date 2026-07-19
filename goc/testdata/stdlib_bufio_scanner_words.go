package main

import (
	"bufio"
	"strings"
)

func main() {
	scanner := bufio.NewScanner(strings.NewReader("one two three"))
	scanner.Split(bufio.ScanWords)

	total := 0
	for scanner.Scan() {
		total += len(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		panic("Scanner returned an error")
	}
	if total != 11 {
		panic("Scanner returned wrong word lengths")
	}
}
