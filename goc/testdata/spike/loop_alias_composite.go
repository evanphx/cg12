package main

type cell struct{ v int }

//go:noinline
func alternate(n int) (int, int) {
	var p, q *cell
	for i := 0; i < n; i++ {
		c := &cell{v: i}
		p = q
		q = c
	}
	if p == nil || q == nil {
		return -1, -1
	}
	return p.v, q.v
}

func main() {
	a, b := alternate(3)
	println("alternate:", a, b)
	if a == b {
		println("ALIASED: the two iterations share one allocation")
	} else {
		println("distinct")
	}
}
