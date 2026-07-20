package main

import "net/url"

func main() {
	base, err := url.Parse("https://example.com/root/path/?a=1")
	if err != nil {
		panic("url base Parse failed")
	}
	relative, err := url.Parse("../next?q=go+compiler&flag=true")
	if err != nil {
		panic("url relative Parse failed")
	}

	resolved := base.ResolveReference(relative)
	if resolved.String() != "https://example.com/root/next?q=go+compiler&flag=true" {
		panic("url ResolveReference mismatch")
	}

	values := resolved.Query()
	if values.Get("q") != "go compiler" {
		panic("url query q mismatch")
	}
	if values.Get("flag") != "true" {
		panic("url query flag mismatch")
	}
}
