package main

type nilAssertionReader interface {
	Read() int
}

type nilAssertionResource struct {
	value int
}

func (resource *nilAssertionResource) Read() int {
	return resource.value
}

func main() {
	var empty any
	if empty != nil {
		panic("empty interface should be nil")
	}
	if _, ok := empty.(nilAssertionReader); ok {
		panic("nil empty interface assertion succeeded")
	}

	var resource *nilAssertionResource
	var reader nilAssertionReader = resource
	if reader == nil {
		panic("interface holding typed nil pointer compared equal to nil")
	}

	recovered, ok := any(reader).(nilAssertionReader)
	if !ok || recovered != reader {
		panic("typed nil interface assertion failed")
	}
}
