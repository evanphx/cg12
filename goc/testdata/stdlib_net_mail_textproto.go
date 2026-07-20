package main

import (
	"net/mail"
	"net/textproto"
)

func main() {
	address, err := mail.ParseAddress(`Gopher <gopher@example.com>`)
	if err != nil {
		panic("mail ParseAddress failed")
	}
	if address.Name != "Gopher" {
		panic("mail address name mismatch")
	}
	if address.Address != "gopher@example.com" {
		panic("mail address mismatch")
	}

	header := textproto.MIMEHeader{}
	header.Add("Content-Type", "text/plain")
	header.Add("X-Status", "alpha")
	header.Add("X-Status", "beta")
	if header.Get("content-type") != "text/plain" {
		panic("textproto canonical get mismatch")
	}
	values := header.Values("x-status")
	if len(values) != 2 || values[0] != "alpha" || values[1] != "beta" {
		panic("textproto values mismatch")
	}
}
