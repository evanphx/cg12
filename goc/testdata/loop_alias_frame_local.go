package main

// The other direction of the loop-body allocation question. Every allocation
// here is inside a loop and every one of them is finished with before the
// iteration ends, so one frame slot is all any of them needs -- which is also
// what the host toolchain gives them.
//
// A rule that heaps allocations in loop bodies without asking whether anything
// retains them would put all three of these on the heap and nothing would
// fail. That is why this program is in the corpus: the allocation census reads
// it and would gain a runtime.newobject line for each one.

type point struct{ x, y int }

//go:noinline
func addTo(p *[2]int, v int) { p[0] += v; p[1] += 2 * v }

// framed takes the address of a loop-body array, hands it to a callee that
// does not retain it, and takes it again into a loop-body local.
//
//go:noinline
func framed(n int) int {
	total := 0
	for i := 0; i < n; i++ {
		var a [2]int
		addTo(&a, i)
		q := &a
		total += q[0] + q[1]
	}
	return total
}

// consumedWithin keeps the pointer in a local declared by the same iteration.
//
//go:noinline
func consumedWithin(n int) int {
	total := 0
	for i := 0; i < n; i++ {
		x := i * 2
		p := &x
		total += *p
	}
	return total
}

// literalWithin is the composite-literal form, read and dropped in the same
// iteration.
//
//go:noinline
func literalWithin(n int) int {
	total := 0
	for i := 0; i < n; i++ {
		p := &point{x: i, y: i * 2}
		total += p.x + p.y
	}
	return total
}

func main() {
	println("framed: ", framed(4))
	println("within: ", consumedWithin(4))
	println("literal:", literalWithin(4))
}
