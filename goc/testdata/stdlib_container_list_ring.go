package main

import (
	"container/list"
	"container/ring"
)

func main() {
	values := list.New()
	first := values.PushBack(10)
	values.PushBack(20)
	values.PushFront(5)
	values.InsertAfter(15, first)

	sum := 0
	for entry := values.Front(); entry != nil; entry = entry.Next() {
		sum += entry.Value.(int)
	}
	if sum != 50 {
		panic("list sum mismatch")
	}

	circle := ring.New(4)
	for i := 0; i < circle.Len(); i++ {
		circle.Value = i + 1
		circle = circle.Next()
	}

	ringSum := 0
	circle.Do(func(value any) {
		ringSum += value.(int)
	})
	if ringSum != 10 {
		panic("ring sum mismatch")
	}
}
