package main

import "strconv"

func main() {
	integerText := strconv.Itoa(-12345)
	if integerText != "-12345" {
		panic("Itoa returned wrong text")
	}
	integer, err := strconv.Atoi(integerText)
	if err != nil {
		panic("Atoi failed")
	}
	if integer != -12345 {
		panic("Atoi returned wrong value")
	}

	quoted := strconv.Quote("hello\n世界")
	if quoted != "\"hello\\n世界\"" {
		panic("Quote returned wrong text")
	}
	unquoted, err := strconv.Unquote(quoted)
	if err != nil {
		panic("Unquote failed")
	}
	if unquoted != "hello\n世界" {
		panic("Unquote returned wrong value")
	}

	floating := strconv.FormatFloat(3.5, 'f', 1, 64)
	if floating != "3.5" {
		panic("FormatFloat returned wrong value")
	}
	parsed, err := strconv.ParseFloat(floating, 64)
	if err != nil {
		panic("ParseFloat failed")
	}
	if parsed != 3.5 {
		panic("ParseFloat returned wrong value")
	}
}
