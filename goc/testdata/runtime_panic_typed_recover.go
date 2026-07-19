package main

type panicValue struct {
	left  int
	right int
}

func trigger() (result int) {
	defer func() {
		value := recover()
		payload, ok := value.(panicValue)
		if !ok {
			panic("recovered value has the wrong dynamic type")
		}
		result = payload.left + payload.right
	}()

	panic(panicValue{left: 17, right: 25})
}

func main() {
	if trigger() != 42 {
		panic("typed recover result mismatch")
	}
}
