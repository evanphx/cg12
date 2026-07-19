package main

func main() {
	text := "abcdef"
	bytes := []byte(text)
	bytes[1] = 'Z'

	if text != "abcdef" {
		panic("byte slice mutation changed original string")
	}

	result := string(bytes)
	if result != "aZcdef" {
		panic("string byte round trip produced wrong result")
	}

	runes := []rune("go語")
	if string(runes) != "go語" {
		panic("rune string round trip failed")
	}
}
