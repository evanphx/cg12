package main

import "unicode/utf8"

func main() {
	text := "a世界"
	if utf8.RuneCountInString(text) != 3 {
		panic("utf8 RuneCountInString mismatch")
	}

	first, size := utf8.DecodeRuneInString(text)
	if first != 'a' || size != 1 {
		panic("utf8 DecodeRuneInString first rune mismatch")
	}

	last, size := utf8.DecodeLastRuneInString(text)
	if last != '界' || size != 3 {
		panic("utf8 DecodeLastRuneInString last rune mismatch")
	}
}
