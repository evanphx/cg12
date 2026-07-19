package main

type methodValueCounter interface {
	Add(int) int
}

type methodValueState struct {
	base int
}

func (state *methodValueState) Add(value int) int {
	state.base += value
	return state.base
}

func main() {
	var counter methodValueCounter = &methodValueState{base: 30}
	add := counter.Add

	if add(5) != 35 {
		panic("first interface method value call failed")
	}
	if add(7) != 42 {
		panic("second interface method value call failed")
	}
}
