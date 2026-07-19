package main

func main() {
	if recover() != nil {
		panic("recover outside panic returned a value")
	}

	func() {
		defer func() {
			if recover() != nil {
				panic("recover in non-panicking defer returned a value")
			}
		}()
	}()
}
