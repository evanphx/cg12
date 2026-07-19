package main

func appendValue(target *[]int, value int) {
	*target = append(*target, value)
}

func collect() (values []int) {
	values = []int{}
	current := 1
	defer appendValue(&values, current)
	current = 2
	defer appendValue(&values, current)
	current = 3
	return
}

func main() {
	values := collect()
	if len(values) != 2 {
		panic("wrong deferred value count")
	}
	if values[0] != 2 || values[1] != 1 {
		panic("defer arguments were not evaluated immediately")
	}
}
