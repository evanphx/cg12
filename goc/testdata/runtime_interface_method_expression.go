package main

type methodExpressionInterface interface {
	Value(int) int
}

type methodExpressionBox struct {
	base int
}

func (box *methodExpressionBox) Value(delta int) int {
	return box.base + delta
}

func main() {
	var iface methodExpressionInterface = &methodExpressionBox{base: 37}
	method := methodExpressionInterface.Value
	if method(iface, 5) != 42 {
		panic("interface method expression returned wrong value")
	}
}
