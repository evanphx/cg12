package main

type panicStructValue struct {
	left  int
	right int
}

func main() {
	defer func() {
		value := recover()
		typed, ok := value.(panicStructValue)
		if !ok {
			panic("recovered value had wrong type")
		}
		if typed.left+typed.right != 42 {
			panic("recovered struct value was corrupted")
		}
	}()

	panic(panicStructValue{left: 17, right: 25})
}
