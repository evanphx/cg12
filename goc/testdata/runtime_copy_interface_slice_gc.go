package main

import "runtime"

type copyInterfaceBox struct {
	value int
	next  *copyInterfaceBox
}

func main() {
	source := make([]interface{}, 60)
	for index := 0; index < len(source); index++ {
		if index%2 == 0 {
			source[index] = index
		} else {
			source[index] = &copyInterfaceBox{
				value: index,
				next:  &copyInterfaceBox{value: index * 2},
			}
		}
	}

	destination := make([]interface{}, len(source))
	if copy(destination, source) != len(source) {
		panic("copy returned wrong count")
	}
	source = nil

	for attempt := 0; attempt < 64; attempt++ {
		runtime.GC()
	}

	total := 0
	for _, value := range destination {
		switch typed := value.(type) {
		case int:
			total += typed
		case *copyInterfaceBox:
			total += typed.value
			total += typed.next.value
		default:
			panic("copied unexpected interface value")
		}
	}
	if total != 3570 {
		panic("copied interface slice was corrupted by GC")
	}
}
