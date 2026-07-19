package main

type nilPointerBox struct {
	value int
}

func main() {
	var pointer *nilPointerBox
	var value interface{} = pointer

	if value == nil {
		panic("typed nil pointer interface compared equal to nil")
	}

	recovered, ok := value.(*nilPointerBox)
	if !ok {
		panic("typed nil pointer assertion failed")
	}
	if recovered != nil {
		panic("typed nil pointer assertion returned non-nil pointer")
	}
}
