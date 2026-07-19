package main

type callbackBox struct {
	value int
}

func callback(arg any, seq uintptr, delay int64) {
	box := arg.(*callbackBox)
	box.value += int(seq)
	if delay < 0 {
		panic("negative delay")
	}
}

func main() {
	box := &callbackBox{value: 10}
	var arg any = box
	var f func(any, uintptr, int64) = callback
	f(arg, 32, 0)
	if box.value != 42 {
		panic("callback did not update box")
	}
}
