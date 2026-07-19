package main

import "unsafe"

type unsafeFieldNode struct {
	left  int32
	right int64
	next  *unsafeFieldNode
}

func main() {
	node := &unsafeFieldNode{
		left:  7,
		right: 11,
		next:  &unsafeFieldNode{left: 13, right: 17},
	}

	base := unsafe.Pointer(node)
	right := (*int64)(unsafe.Pointer(uintptr(base) + unsafe.Offsetof(node.right)))
	*right = 25

	next := (**unsafeFieldNode)(unsafe.Pointer(uintptr(base) + unsafe.Offsetof(node.next)))
	(*next).right = 31

	if int64(node.left)+node.right+int64(node.next.left)+node.next.right != 76 {
		panic("unsafe struct field access produced wrong values")
	}
}
