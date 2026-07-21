package main

import "net/url"

func main() {
	values := url.Values{}
	values.Add("q", "go compiler")
	values.Add("tag", "cg12")
	values.Add("tag", "runtime")

	encoded := values.Encode()
	if encoded != "q=go+compiler&tag=cg12&tag=runtime" {
		panic("url.Values Encode mismatch")
	}

	parsed, err := url.ParseQuery(encoded)
	if err != nil {
		panic("url ParseQuery failed")
	}
	if parsed.Get("q") != "go compiler" {
		panic("url Values q mismatch")
	}
	if len(parsed["tag"]) != 2 || parsed["tag"][1] != "runtime" {
		panic("url Values repeated key mismatch")
	}
}
