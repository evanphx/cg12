package main

type comparableKey struct {
	name  string
	index int
}

func main() {
	table := map[interface{}]int{
		comparableKey{name: "left", index: 1}:  17,
		comparableKey{name: "right", index: 2}: 25,
		"extra":                                100,
	}

	total := table[comparableKey{name: "left", index: 1}]
	total += table[comparableKey{name: "right", index: 2}]
	delete(table, "extra")

	if total != 42 {
		panic("interface-keyed map lookup mismatch")
	}
	if len(table) != 2 {
		panic("interface-keyed map delete mismatch")
	}
}
