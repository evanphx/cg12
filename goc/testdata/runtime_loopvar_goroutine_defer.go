package main

import (
	"fmt"
	"sort"
	"sync"
)

// A goroutine started in a loop and a closure deferred in a loop are the two
// ways per-iteration scoping is most often observed. The deferred case also
// exercises the rule that a defer statement which can register more than once
// gets a fresh closure descriptor and heap-lifted captures per registration.
func main() {
	goroutinesSeeTheirOwnIteration()
	deferredClosuresSeeTheirOwnIteration()
	deferredRangeClosuresSeeTheirOwnIteration()
	nestedDefersSeeTheirOwnIteration()
}

func goroutinesSeeTheirOwnIteration() {
	var group sync.WaitGroup
	squares := make([]int, 4)
	for index := 0; index < 4; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			squares[index] = index * index
		}()
	}
	group.Wait()
	if fmt.Sprint(squares) != "[0 1 4 9]" {
		panic("goroutines shared one loop variable: " + fmt.Sprint(squares))
	}

	group = sync.WaitGroup{}
	var mutex sync.Mutex
	var seen []string
	for key, value := range map[string]int{"a": 1, "b": 2, "c": 3} {
		group.Add(1)
		go func() {
			defer group.Done()
			mutex.Lock()
			seen = append(seen, key+fmt.Sprint(value))
			mutex.Unlock()
		}()
	}
	group.Wait()
	sort.Strings(seen)
	if fmt.Sprint(seen) != "[a1 b2 c3]" {
		panic("goroutines shared one map range variable: " + fmt.Sprint(seen))
	}
}

func deferredClosuresSeeTheirOwnIteration() {
	var order []int
	defer func() {
		if fmt.Sprint(order) != "[2 1 0]" {
			panic("deferred closures shared one loop variable: " + fmt.Sprint(order))
		}
	}()
	for index := 0; index < 3; index++ {
		defer func() {
			order = append(order, index)
		}()
	}
}

func deferredRangeClosuresSeeTheirOwnIteration() {
	var order []string
	defer func() {
		if fmt.Sprint(order) != "[c b a]" {
			panic("deferred closures shared one range variable: " + fmt.Sprint(order))
		}
	}()
	for _, letter := range []string{"a", "b", "c"} {
		defer func() {
			order = append(order, letter)
		}()
	}
}

func nestedDefersSeeTheirOwnIteration() {
	var order []int
	for outer := 0; outer < 2; outer++ {
		func() {
			for inner := 0; inner < 2; inner++ {
				defer func() {
					order = append(order, outer*10+inner)
				}()
			}
		}()
	}
	if fmt.Sprint(order) != "[1 0 11 10]" {
		panic("nested defers shared one loop variable: " + fmt.Sprint(order))
	}
}
