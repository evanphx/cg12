// Command jsonbench times an encoding/json round trip, which is reflection,
// interface dispatch and a hand-written scanner rather than arithmetic.
package main

import (
	"encoding/json"
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

type record struct {
	ID       int               `json:"id"`
	Name     string            `json:"name"`
	Tags     []string          `json:"tags"`
	Scores   []float64         `json:"scores"`
	Metadata map[string]string `json:"metadata"`
	Nested   []child           `json:"nested"`
	Active   bool              `json:"active"`
}

type child struct {
	Key   string `json:"key"`
	Value int    `json:"value"`
}

func buildDocument() []record {
	records := make([]record, 400)
	for i := range records {
		records[i] = record{
			ID:     i,
			Name:   fmt.Sprintf("record-%d-with-a-reasonably-long-name", i),
			Tags:   []string{"alpha", "beta", "gamma", fmt.Sprintf("tag%d", i%17)},
			Scores: []float64{float64(i) * 1.5, float64(i) / 3, 1e9 / float64(i+1)},
			Metadata: map[string]string{
				"region": "eu-west-1",
				"owner":  fmt.Sprintf("team%d", i%9),
				"state":  "ready",
			},
			Nested: []child{{"a", i}, {"b", i * 2}, {"c", i * 3}},
			Active: i%3 == 0,
		}
	}
	return records
}

func main() {
	report("control/spin-fixed-work", control)

	document := buildDocument()
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}

	report("json/marshal", func() {
		for i := 0; i < 16; i++ {
			b, err := json.Marshal(document)
			if err != nil {
				panic(err)
			}
			sink += uint64(len(b))
		}
	})
	report("json/unmarshal", func() {
		for i := 0; i < 8; i++ {
			var decoded []record
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				panic(err)
			}
			sink += uint64(len(decoded))
		}
	})
}
