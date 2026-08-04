// Command floatbench times floating-point arithmetic, which nothing else in
// this suite does.
//
// Every other workload here is integers, pointers and the runtime. Floating
// point is a separate register file, a separate set of instruction latencies and
// a separate set of decisions in a code generator -- whether it fuses a multiply
// and an add, whether it keeps a value in a register across a loop, whether it
// calls out to software for a square root. A backend can be entirely correct on
// integers and leave every one of those on the table, and no other row would
// say so.
//
// The cases are picked so each one fails differently:
//
//   - float/dot-product is a long dependent chain of multiply-adds over two
//     slices. It is the case a fused multiply-add helps most and the case where
//     an extra bounds check on the chain costs most.
//   - float/mandelbrot is arithmetic with a data-dependent branch in the loop,
//     so it cannot be turned into straight-line work.
//   - float/sqrt-sum is math.Sqrt in a loop. On this architecture that is one
//     instruction if the compiler knows it and a function call if it does not,
//     and the two answers differ by a factor, not by a percent.
//   - float/int-convert crosses between the two register files every iteration,
//     which is a move with a real latency and a place where a compiler can
//     accidentally round-trip through memory.
package main

import (
	"fmt"
	"math"
	"time"
)

// rounds is how many timed rounds each case gets; the fastest is reported.
const rounds = 3

// sink keeps the control loop from being optimised away.
var sink uint64

// floatSink is the same for the floating-point cases.
var floatSink float64

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
	// vectorElements is the length of the two slices the dot product runs over.
	// 64 Ki float64s is 512 KiB a side, which stays in cache so the case is
	// bound by arithmetic rather than by memory.
	vectorElements = 64 * 1024
	// dotRepeats is how many times one round runs the dot product.
	dotRepeats = 200
	// mandelbrotSide is the width and height of the grid the escape-time case
	// walks.
	mandelbrotSide = 400
	// mandelbrotLimit is the iteration cap per point.
	mandelbrotLimit = 100
	// sqrtIterations is how many square roots one round takes. It is far below
	// the other counts here for a reason worth reading: goc does not lower
	// math.Sqrt to the hardware instruction, so a square root costs it about
	// 380 ns against the host toolchain's 2.4 ns, and 20_000_000 of them cost
	// the goc-built binary seven and a half seconds a round.
	sqrtIterations = 1_000_000
	// convertIterations is how many int/float round trips one round makes.
	convertIterations = 10_000_000
)

func dotProduct(left, right []float64) float64 {
	total := 0.0
	for i := range left {
		total += left[i] * right[i]
	}
	return total
}

// mandelbrot counts escape-time iterations over a grid. The inner loop's trip
// count depends on the data, so the work cannot be hoisted or unrolled away.
func mandelbrot(side, limit int) int {
	escaped := 0
	for y := 0; y < side; y++ {
		imaginary := 2.0*float64(y)/float64(side) - 1.0
		for x := 0; x < side; x++ {
			real := 3.0*float64(x)/float64(side) - 2.0
			zr, zi := 0.0, 0.0
			iteration := 0
			for ; iteration < limit; iteration++ {
				zr2 := zr * zr
				zi2 := zi * zi
				if zr2+zi2 > 4.0 {
					break
				}
				zi = 2*zr*zi + imaginary
				zr = zr2 - zi2 + real
			}
			escaped += iteration
		}
	}
	return escaped
}

func main() {
	report("control/spin-fixed-work", control)

	left := make([]float64, vectorElements)
	right := make([]float64, vectorElements)
	state := uint64(0x2545F4914F6CDD1D)
	for i := range left {
		state = state*6364136223846793005 + 1442695040888963407
		left[i] = float64(state>>40) / 16777216.0
		state = state*6364136223846793005 + 1442695040888963407
		right[i] = float64(state>>40) / 16777216.0
	}

	report("float/dot-product", func() {
		total := 0.0
		for repeat := 0; repeat < dotRepeats; repeat++ {
			total += dotProduct(left, right)
		}
		floatSink += total
	})

	report("float/mandelbrot", func() {
		sink += uint64(mandelbrot(mandelbrotSide, mandelbrotLimit))
	})

	report("float/sqrt-sum", func() {
		total := 0.0
		for i := 1; i <= sqrtIterations; i++ {
			total += math.Sqrt(float64(i))
		}
		floatSink += total
	})

	report("float/int-convert", func() {
		total := uint64(0)
		value := 1.5
		for i := 0; i < convertIterations; i++ {
			total += uint64(value)
			value = float64(total&1023) + 0.5
		}
		sink += total
	})
}
