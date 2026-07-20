package main

import "container/heap"

type intHeap []int

func (h intHeap) Len() int {
	return len(h)
}

func (h intHeap) Less(i int, j int) bool {
	return h[i] < h[j]
}

func (h intHeap) Swap(i int, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *intHeap) Push(value any) {
	*h = append(*h, value.(int))
}

func (h *intHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func main() {
	values := &intHeap{5, 1, 4}
	heap.Init(values)
	heap.Push(values, 3)
	heap.Push(values, 2)

	expected := []int{1, 2, 3, 4, 5}
	for _, want := range expected {
		got := heap.Pop(values).(int)
		if got != want {
			panic("heap pop order mismatch")
		}
	}
	if values.Len() != 0 {
		panic("heap not empty")
	}
}
