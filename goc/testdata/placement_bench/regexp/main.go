// Command regexpbench times regexp matching, which is a second interpreter --
// the package compiles a pattern to a program and runs it over the input -- with
// a much larger dispatch than interpbench's.
package main

import (
	"fmt"
	"regexp"
	"strings"
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

func main() {
	report("control/spin-fixed-work", control)

	var builder strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&builder, "2024-%02d-%02d host%d sshd[%d]: accepted key for user%d from 10.%d.%d.%d port %d\n",
			i%12+1, i%28+1, i%7, 1000+i, i%50, i%256, (i*7)%256, (i*13)%256, 20000+i)
	}
	corpus := builder.String()

	address := regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)\.(\d+)`)
	entry := regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2}) (\w+) (\w+)\[(\d+)\]: (.*)$`)
	report("regexp/find-submatch", func() {
		for i := 0; i < 2; i++ {
			sink += uint64(len(address.FindAllStringSubmatch(corpus, -1)))
		}
	})
	report("regexp/anchored-lines", func() {
		lines := strings.Split(corpus, "\n")
		for i := 0; i < 4; i++ {
			for _, line := range lines {
				if m := entry.FindStringSubmatch(line); m != nil {
					sink += uint64(len(m))
				}
			}
		}
	})
	report("regexp/replace", func() {
		for i := 0; i < 2; i++ {
			sink += uint64(len(address.ReplaceAllString(corpus, "$1.x.x.$4")))
		}
	})
}
