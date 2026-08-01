// A type's GC pointer mask is read a uintptr at a time, so a mask emitted at its
// exact significant length has the next symbol read as part of it.
//
// runtime.readUintptr loads eight bytes, and every reader of an abi.Type's
// GCData goes through it: typePointersOfType takes the first word as its mask,
// typePointers.next and fastForward take later words, and heapSetType reads it
// to write a new object's heap bitmap. A one-byte mask for a one-pointer type
// therefore picks up seven bytes of whatever the linker placed next, and each 1
// bit in them is a phantom pointer word at an offset outside the object.
//
// This program is the reducer for that. It needs three things at once: a heap
// object with pointers, an allocating expression stored into it so a mark phase
// is running while the graph grows, and an append that has to call growslice --
// growslice is the path that calls bulkBarrierPreWriteSrcOnly with a small size
// and the element type, which is where a phantom bit reaches furthest past the
// end of the source. On the compiler that emitted unpadded masks this dies with
// "found bad pointer in Go heap" on every run; the mask for []*vertex's element
// read as 0x0800000000000001, whose bit 59 is word offset 472.
package main

import "runtime"

type vertex struct {
	value int
	label string
	edges []*vertex
}

//go:noinline
func buildGraph(seed int) *vertex {
	root := &vertex{value: seed, label: "root"}
	frontier := []*vertex{root}
	for depth := 0; depth < 6; depth++ {
		var next []*vertex
		for _, parent := range frontier {
			for child := 0; child < 3; child++ {
				node := &vertex{
					value: parent.value*3 + child,
					label: "node-" + string(rune('a'+(depth+child)%26)),
				}
				parent.edges = append(parent.edges, node)
				next = append(next, node)
			}
		}
		frontier = next
		if len(frontier) > 512 {
			frontier = frontier[:512]
		}
	}
	return root
}

//go:noinline
func sumGraph(root *vertex) int {
	total := root.value
	if root.label == "" {
		panic("a vertex lost its label")
	}
	for _, edge := range root.edges {
		total += sumGraph(edge)
	}
	return total
}

//go:noinline
func churn(rounds int) {
	for round := 0; round < rounds; round++ {
		local := buildGraph(round)
		if local.value != round {
			panic("churn built the wrong graph")
		}
	}
}

func main() {
	retained := buildGraph(0)
	expected := sumGraph(retained)

	done := make(chan bool)
	for worker := 0; worker < 8; worker++ {
		go func() {
			churn(24)
			done <- true
		}()
	}
	for cycle := 0; cycle < 6; cycle++ {
		runtime.GC()
	}
	for worker := 0; worker < 8; worker++ {
		<-done
	}
	runtime.GC()

	if got := sumGraph(retained); got != expected {
		println("graph sums to", got, "want", expected)
		panic("a retained graph changed across concurrent marking")
	}
	println("type mask padding ok")
}
