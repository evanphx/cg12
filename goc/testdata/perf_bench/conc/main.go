// Command concbench times goroutines, channels and mutexes: the parts of a Go
// program whose cost is paid almost entirely inside the runtime rather than in
// compiled user code.
//
// It is here because it is the workload whose answer a code generator has the
// least control over and goc's own runtime has the most. Every other program in
// this suite mostly measures the instructions goc emits; a channel send measures
// goc's scheduler, its lock, and its park/unpark path. Those are the parts most
// likely to be a placeholder implementation, and nothing else in this tree
// measures how much one costs.
//
// Everything is deliberately single-threaded in the sense that matters: the
// suite pins its runs to one core, so these cases measure the cost of the
// runtime's bookkeeping and its context switches, not parallel speedup. That is
// the property worth watching, and it is the one that is reproducible.
package main

import (
	"fmt"
	"sync"
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

const (
	// pingPongs is how many round trips the unbuffered channel case makes. Each
	// one is two sends, two receives and at least two goroutine switches.
	pingPongs = 30_000
	// bufferedSends is how many sends the buffered case makes. A send into a
	// buffer with room does not switch goroutines, so the pair of cases splits
	// the channel's own bookkeeping from the scheduler's.
	bufferedSends = 150_000
	// spawns is how many goroutines the creation case starts and joins. It is
	// the smallest count here because it is the case goc is slowest at by a
	// wide margin: 100_000 of them cost the goc-built binary 2.4 seconds a
	// round, which the suite cannot afford ten repetitions of.
	spawns = 20_000
	// lockPairs is how many uncontended Lock/Unlock pairs the mutex case makes.
	lockPairs = 2_000_000
)

func main() {
	report("control/spin-fixed-work", control)

	// Unbuffered: every send blocks until the other side receives, so this is
	// the scheduler's park/unpark path as much as it is the channel's.
	report("chan/pingpong-unbuffered", func() {
		forward := make(chan int)
		back := make(chan int)
		done := make(chan struct{})
		go func() {
			for value := range forward {
				back <- value + 1
			}
			close(done)
		}()
		total := 0
		for i := 0; i < pingPongs; i++ {
			forward <- i
			total += <-back
		}
		close(forward)
		<-done
		sink += uint64(total)
	})

	// Buffered with room: the send path without the switch.
	report("chan/send-buffered", func() {
		values := make(chan int, 1024)
		done := make(chan struct{})
		go func() {
			total := 0
			for value := range values {
				total += value
			}
			sink += uint64(total)
			close(done)
		}()
		for i := 0; i < bufferedSends; i++ {
			values <- i
		}
		close(values)
		<-done
	})

	// Creation and join. The body is trivial on purpose: what is being measured
	// is the cost of starting a goroutine and of the WaitGroup that collects it.
	report("goroutine/spawn-join", func() {
		var group sync.WaitGroup
		var counter [64]uint64
		for i := 0; i < spawns; i++ {
			group.Add(1)
			go func(index int) {
				counter[index&63] += uint64(index)
				group.Done()
			}(i)
		}
		group.Wait()
		sink += counter[0]
	})

	// Uncontended locking. One goroutine, so the fast path is all that runs, and
	// the case says what an atomic compare-and-swap plus the surrounding code
	// costs under each compiler.
	report("mutex/uncontended", func() {
		var lock sync.Mutex
		total := 0
		for i := 0; i < lockPairs; i++ {
			lock.Lock()
			total += i
			lock.Unlock()
		}
		sink += uint64(total)
	})
}
