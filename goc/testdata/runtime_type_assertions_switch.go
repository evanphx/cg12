package main

type reader interface {
	read() int
}

type boxed struct {
	value int
}

func (box boxed) read() int {
	return box.value
}

func classify(value interface{}) int {
	switch item := value.(type) {
	case nil:
		return 1
	case int:
		return item + 2
	case boxed:
		return item.read() + 3
	case reader:
		return item.read() + 4
	default:
		return 5
	}
}

func main() {
	total := 0
	total += classify(nil)
	total += classify(10)
	total += classify(boxed{value: 20})
	var value interface{} = &boxed{value: 30}
	total += classify(value)
	total += classify("fallback")

	if total != 75 {
		panic("type switch result mismatch")
	}

	if box, ok := value.(reader); !ok || box.read() != 30 {
		panic("interface assertion failed")
	}
}
