package main

// The address of a per-iteration loop variable, appended to a slice that
// outlives the loop. cmd/compile says `moved to heap: index`; goc's census says
// frame at the same position, because goc records the frame decision at the
// loop header and a separate positionless heap allocation for the per-iteration
// copy. A positionless row cannot join on position, which is how a line reads
// as "goc frames what gc heaps" while goc heaps the thing that escapes.
func main() {
	var pointers []*int
	for index := 0; index < 4; index++ {
		pointers = append(pointers, &index)
	}
	if pointers[0] == pointers[1] {
		println("SHARED")
	} else {
		println("DISTINCT")
	}
	println(*pointers[0], *pointers[1], *pointers[2], *pointers[3])
}
