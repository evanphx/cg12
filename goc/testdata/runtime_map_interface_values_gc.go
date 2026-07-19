package main

import "runtime"

type mapInterfaceValue struct {
	base int
	next *mapInterfaceValue
}

func main() {
	values := make(map[int]interface{})
	for index := 0; index < 96; index++ {
		values[index] = &mapInterfaceValue{
			base: index,
			next: &mapInterfaceValue{base: index + 5},
		}
	}
	for index := 0; index < 96; index += 4 {
		values[index] = index * 7
	}

	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
	}

	total := 0
	for index := 0; index < 96; index++ {
		switch value := values[index].(type) {
		case int:
			total += value
		case *mapInterfaceValue:
			total += value.base
			total += value.next.base
		default:
			panic("unexpected map interface value type")
		}
	}

	if total != 15000 {
		panic("map interface values were corrupted by GC")
	}
}
