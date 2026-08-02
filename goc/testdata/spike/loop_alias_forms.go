package main

//go:noinline
func viaNew(n int) (int, int) {
	var p, q *int
	for i := 0; i < n; i++ {
		x := new(int)
		*x = i
		p = q
		q = x
	}
	if p == nil || q == nil {
		return -1, -1
	}
	return *p, *q
}

//go:noinline
func viaMake(n int) (int, int) {
	var p, q []int
	for i := 0; i < n; i++ {
		b := make([]int, 0, 4)
		b = append(b, i)
		p = q
		q = b
	}
	if p == nil || q == nil {
		return -1, -1
	}
	return p[0], q[0]
}

//go:noinline
func viaArray(n int) (int, int) {
	var p, q *[2]int
	for i := 0; i < n; i++ {
		var a [2]int
		a[0] = i
		p = q
		q = &a
	}
	if p == nil || q == nil {
		return -1, -1
	}
	return p[0], q[0]
}

func main() {
	a, b := viaNew(3)
	println("new:  ", a, b)
	c, d := viaMake(3)
	println("make: ", c, d)
	e, f := viaArray(3)
	println("array:", e, f)
}
