// Command textbench times number formatting and parsing and string building --
// strconv's digit loops, fmt's reflective dispatch, and utf8 decoding.
package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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

func main() {
	report("control/spin-fixed-work", control)

	numbers := make([]int64, 60_000)
	state := uint64(999331)
	for i := range numbers {
		state = state*6364136223846793005 + 1442695040888963407
		numbers[i] = int64(state >> 20)
	}
	decimals := make([]string, len(numbers))
	for i, n := range numbers {
		decimals[i] = strconv.FormatInt(n, 10)
	}
	prose := strings.Repeat("the quick brown fox jumps over the lazy dog — ζωή, 日本語, ñandú. ", 4000)

	report("text/format-append", func() {
		for i := 0; i < 3; i++ {
			var builder strings.Builder
			for _, n := range numbers {
				builder.WriteString(strconv.FormatInt(n, 10))
				builder.WriteByte(',')
			}
			sink += uint64(builder.Len())
		}
	})
	report("text/parse", func() {
		for i := 0; i < 8; i++ {
			for _, d := range decimals {
				v, err := strconv.ParseInt(d, 10, 64)
				if err != nil {
					panic(err)
				}
				sink += uint64(v)
			}
		}
	})
	report("text/sprintf", func() {
		for i := 0; i < 1; i++ {
			for j, n := range numbers {
				sink += uint64(len(fmt.Sprintf("%s=%d/%x", decimals[j], n, n)))
			}
		}
	})
	report("text/utf8-decode", func() {
		for i := 0; i < 30; i++ {
			for _, r := range prose {
				sink += uint64(r)
			}
			sink += uint64(utf8.RuneCountInString(prose))
		}
	})
}
