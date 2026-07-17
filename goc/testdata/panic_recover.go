package main

func trigger() {
	defer func() {
		if recover() == nil {
			panic("missing panic")
		}
	}()
	panic("boom")
}

func main() {
	trigger()
	println("recovered")
}
