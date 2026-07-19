package main

func main() {
	recoveredNil := false

	func() {
		defer func() {
			recovered := recover()
			if recovered == nil {
				recoveredNil = true
			}
		}()
		panic(nil)
	}()

	if recoveredNil {
		panic("panic(nil) recovered as nil")
	}
}
