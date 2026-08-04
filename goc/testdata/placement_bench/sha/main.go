// Command shabench times SHA-256 and HMAC-SHA-256 over a fixed buffer. Under goc
// the compression function is compiled Go rather than assembly, so it is one
// tight block loop with no call in it -- the simplest shape a fetch granule can
// matter to.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
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

const bufferSize = 1 << 20

func main() {
	report("control/spin-fixed-work", control)

	buffer := make([]byte, bufferSize)
	for i := range buffer {
		buffer[i] = byte(i*31 + 7)
	}
	key := []byte("cg12 placement benchmark key")

	report("sha/sha256-1mib", func() {
		for i := 0; i < 120; i++ {
			digest := sha256.Sum256(buffer)
			sink += uint64(digest[0])
		}
	})
	report("sha/hmac-1mib", func() {
		for i := 0; i < 72; i++ {
			mac := hmac.New(sha256.New, key)
			mac.Write(buffer)
			sink += uint64(mac.Sum(nil)[0])
		}
	})
}
