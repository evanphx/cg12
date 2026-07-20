package main

import "strings"

func main() {
	fields := strings.Fields(" alpha\tbeta\n世界 ")
	if len(fields) != 3 || fields[2] != "世界" {
		panic("Fields returned wrong values")
	}

	joined := strings.Join(fields, "|")
	if joined != "alpha|beta|世界" {
		panic("Join returned wrong value")
	}

	replacer := strings.NewReplacer("alpha", "A", "世界", "world")
	replaced := replacer.Replace(joined)
	if replaced != "A|beta|world" {
		panic("Replacer returned wrong value")
	}

	mapped := strings.Map(func(r rune) rune {
		if r == '|' {
			return '/'
		}
		return r
	}, replaced)
	if mapped != "A/beta/world" {
		panic("Map returned wrong value")
	}
}
