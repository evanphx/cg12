package main

import (
	"encoding"
	"net/netip"
	"reflect"
)

func main() {
	addressType := reflect.TypeOf(netip.Addr{})
	textMarshalerType := reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()

	println("Addr methods:", addressType.NumMethod())
	println("implements encoding.TextMarshaler:", addressType.Implements(textMarshalerType))
}
