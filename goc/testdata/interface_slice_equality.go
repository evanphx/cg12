package main

func main() {
	values := []any{1}
	value := 1
	if value != values[0] {
		panic("equal interface values compared unequal")
	}
	if value == any(2) {
		panic("different interface values compared equal")
	}
}
