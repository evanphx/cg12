package main

import "runtime"

type typeSwitchLeft struct {
	value int
	next  *typeSwitchRight
}

type typeSwitchRight struct {
	value int
}

func main() {
	values := make([]interface{}, 0, 96)
	for index := 0; index < 32; index++ {
		values = append(values, index)
		values = append(values, &typeSwitchLeft{
			value: index,
			next:  &typeSwitchRight{value: index * 2},
		})
		values = append(values, &typeSwitchRight{value: index * 3})
	}

	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
	}

	total := 0
	for _, value := range values {
		switch typed := value.(type) {
		case int:
			total += typed
		case *typeSwitchLeft:
			total += typed.value
			total += typed.next.value
		case *typeSwitchRight:
			total += typed.value
		default:
			panic("unexpected type switch value")
		}
	}
	if total != 3472 {
		panic("type switch pointer values were corrupted")
	}
}
