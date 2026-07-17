package main

import "testing"

func main() {
	value := 0
	for run := 0; run < 25; run++ {
		println("run", run)
		allocations := testing.AllocsPerRun(100, func() {
			value++
		})
		println("allocations", allocations)
	}
	println("value", value)
}
