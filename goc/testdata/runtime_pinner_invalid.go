package main

import "runtime"

func expectPinPanic(value any) {
	deferredRan := false
	func() {
		defer func() {
			if recover() == nil {
				panic("Pinner.Pin did not reject an invalid argument")
			}
			deferredRan = true
		}()

		var pinner runtime.Pinner
		pinner.Pin(value)
	}()
	if !deferredRan {
		panic("Pinner.Pin panic was not recovered")
	}
}

func main() {
	expectPinPanic(nil)
	expectPinPanic(42)
}
