package main

import "regexp"

func main() {
	expression := regexp.MustCompile(`([a-z]+)-([0-9]+)`)
	match := expression.FindStringSubmatch("left alpha-17 right")
	if len(match) != 3 || match[1] != "alpha" || match[2] != "17" {
		panic("regexp submatch failed")
	}

	replaced := expression.ReplaceAllString("alpha-17 beta-25", "$2:$1")
	if replaced != "17:alpha 25:beta" {
		panic("regexp replacement failed")
	}

	parts := expression.Split("alpha-17 middle beta-25", -1)
	if len(parts) != 3 || parts[1] != " middle " {
		panic("regexp split failed")
	}
}
