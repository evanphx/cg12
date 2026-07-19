package main

func main() {
	table := map[[3]int]string{
		{1, 2, 3}: "left",
		{4, 5, 6}: "right",
	}

	if table[[3]int{1, 2, 3}] != "left" {
		panic("array map key lookup failed")
	}
	if table[[3]int{4, 5, 6}] != "right" {
		panic("second array map key lookup failed")
	}
	if _, ok := table[[3]int{7, 8, 9}]; ok {
		panic("missing array key was reported as present")
	}
}
