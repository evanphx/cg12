// Command sortmapbench times sorting through a comparison callback and building
// and probing maps: indirect calls and the runtime's hash paths, neither of
// which is a loop the compiler can see all of.
package main

import (
	"fmt"
	"sort"
	"time"
)

// rounds is how many timed rounds each case gets; the fastest is reported.
const rounds = 3

// sink keeps the control loop from being optimised away.
var sink uint64

// control is the fixed amount of integer arithmetic every case is divided by,
// so the machine's speed and its load divide out of the reported index. It is
// the same loop the crypto signing benchmark uses.
func control() {
	accumulator := uint64(1)
	for i := 0; i < 20_000_000; i++ {
		accumulator = accumulator*6364136223846793005 + 1442695040888963407
	}
	sink = accumulator
}

// measure returns the fastest of rounds timed rounds, in nanoseconds. Noise can
// only ever make a round slower, so the fastest is the least contaminated.
func measure(body func()) time.Duration {
	body()
	best := time.Duration(1<<63 - 1)
	for round := 0; round < rounds; round++ {
		start := time.Now()
		body()
		if elapsed := time.Since(start); elapsed < best {
			best = elapsed
		}
	}
	return best
}

func report(name string, body func()) {
	fmt.Printf("%s\t%d\n", name, int64(measure(body)))
}

const elements = 200_000

func main() {
	report("control/spin-fixed-work", control)

	numbers := make([]int, elements)
	state := uint64(12345)
	for i := range numbers {
		state = state*6364136223846793005 + 1442695040888963407
		numbers[i] = int(state >> 33)
	}
	words := make([]string, 40_000)
	for i := range words {
		words[i] = fmt.Sprintf("key-%08d-%d", (i*7919)%len(words), i%97)
	}
	scratch := make([]int, elements)

	report("sort/ints", func() {
		for i := 0; i < 4; i++ {
			copy(scratch, numbers)
			sort.Ints(scratch)
			sink += uint64(scratch[0])
		}
	})
	report("sort/slice-callback", func() {
		for i := 0; i < 3; i++ {
			buffer := append([]string(nil), words...)
			sort.Slice(buffer, func(a, b int) bool { return buffer[a] < buffer[b] })
			sink += uint64(len(buffer[0]))
		}
	})
	report("map/build-probe", func() {
		for i := 0; i < 6; i++ {
			table := make(map[string]int, len(words))
			for j, w := range words {
				table[w] = j
			}
			for _, w := range words {
				sink += uint64(table[w])
			}
		}
	})
}
