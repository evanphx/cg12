package main

func main() {
	order := 0
	recovered := false

	func() {
		defer func() {
			if order != 12 {
				panic("defer order before recover mismatch")
			}
			recovered = recover() != nil
			order = order*10 + 3
		}()
		defer func() {
			order = order*10 + 2
		}()
		defer func() {
			order = order*10 + 1
		}()
		panic("ordered panic")
	}()

	if !recovered || order != 123 {
		panic("defer order panic result mismatch")
	}
}
