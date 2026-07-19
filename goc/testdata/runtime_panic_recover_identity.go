package main

type panicIdentityBox struct {
	value int
}

func main() {
	box := &panicIdentityBox{value: 42}
	value := recoverIdentity(box)
	if value != 42 {
		panic("recover identity mismatch")
	}
}

func recoverIdentity(box *panicIdentityBox) (value int) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			value = -1
			return
		}
		recoveredBox, ok := recovered.(*panicIdentityBox)
		if !ok || recoveredBox != box {
			value = -2
			return
		}
		value = recoveredBox.value
	}()
	panic(box)
}
