// Command flatebench times a compress/flate round trip: table-driven loops over
// a window, with a match search that is all data-dependent branches.
package main

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
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

func buildInput() []byte {
	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta"}
	var buffer bytes.Buffer
	for i := 0; buffer.Len() < 1<<20; i++ {
		buffer.WriteString(words[i%len(words)])
		buffer.WriteByte(' ')
		if i%13 == 0 {
			fmt.Fprintf(&buffer, "%d-%x ", i, i*2654435761)
		}
		if i%64 == 0 {
			buffer.WriteByte('\n')
		}
	}
	return buffer.Bytes()
}

func main() {
	report("control/spin-fixed-work", control)

	input := buildInput()
	var compressed bytes.Buffer
	writer, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		panic(err)
	}
	if _, err := writer.Write(input); err != nil {
		panic(err)
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}
	packed := compressed.Bytes()

	report("flate/compress", func() {
		for i := 0; i < 3; i++ {
			var out bytes.Buffer
			w, err := flate.NewWriter(&out, flate.DefaultCompression)
			if err != nil {
				panic(err)
			}
			if _, err := w.Write(input); err != nil {
				panic(err)
			}
			if err := w.Close(); err != nil {
				panic(err)
			}
			sink += uint64(out.Len())
		}
	})
	report("flate/decompress", func() {
		for i := 0; i < 20; i++ {
			n, err := io.Copy(io.Discard, flate.NewReader(bytes.NewReader(packed)))
			if err != nil {
				panic(err)
			}
			sink += uint64(n)
		}
	})
}
