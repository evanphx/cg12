package main

func inner() {
	defer func() {
		if recover() != "first" {
			panic("wrong recovered panic")
		}
		panic("second")
	}()
	panic("first")
}

func main() {
	defer func() {
		if recover() != "second" {
			panic("repanic was not observed")
		}
	}()
	inner()
}
