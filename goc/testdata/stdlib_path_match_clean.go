package main

import "path"

func main() {
	cleaned := path.Clean("/alpha/./beta/../gamma//")
	if cleaned != "/alpha/gamma" {
		panic("path Clean mismatch")
	}

	matched, err := path.Match("alpha/*/gamma", "alpha/beta/gamma")
	if err != nil {
		panic("path Match returned an error")
	}
	if !matched {
		panic("path Match failed")
	}

	matched, err = path.Match("alpha/?eta/gamma", "alpha/beta/gamma")
	if err != nil {
		panic("path Match wildcard returned an error")
	}
	if !matched {
		panic("path Match single wildcard failed")
	}
}
