#!/usr/bin/env python3
"""Writes the corpus's eight programs, each with the shared harness inlined.

Every program is one self-contained main package because that is what goc
compiles: `goc -O -o out main.go`. The harness is the same twelve lines in all
eight, so it is written from here rather than copied by hand eight times.
"""

import os

HARNESS = '''
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
	fmt.Printf("%s\\t%d\\n", name, int64(measure(body)))
}
'''

PROGRAMS = {}

PROGRAMS['p256'] = ('''// Command p256bench times ECDSA P-256 sign+verify, which is bigmod limb
// arithmetic: the workload the crypto signing benchmark's headline row measures,
// trimmed to one case so a placement sweep can build and run it many times.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"time"
)
''', '''
const signVerifyRounds = 24

func main() {
	report("control/spin-fixed-work", control)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256([]byte("cg12 placement benchmark"))
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		panic(err)
	}

	report("p256/sign-verify", func() {
		for i := 0; i < signVerifyRounds; i++ {
			s, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
			if err != nil {
				panic(err)
			}
			if !ecdsa.VerifyASN1(&key.PublicKey, digest[:], s) {
				panic("signature did not verify")
			}
		}
	})
	report("p256/verify", func() {
		for i := 0; i < signVerifyRounds; i++ {
			if !ecdsa.VerifyASN1(&key.PublicKey, digest[:], signature) {
				panic("signature did not verify")
			}
		}
	})
}
''')

PROGRAMS['sha'] = ('''// Command shabench times SHA-256 and HMAC-SHA-256 over a fixed buffer. Under goc
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
''', '''
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
''')

PROGRAMS['interp'] = ('''// Command interpbench times a bytecode interpreter: a switch over an opcode in a
// loop, which is the classic shape whose speed depends on where the dispatch
// lands. The program it runs is fixed, so the case does exactly the same work
// every time.
package main

import (
	"fmt"
	"time"
)
''', '''
const (
	opPush = iota
	opLoad
	opStore
	opAdd
	opSub
	opMul
	opXor
	opLess
	opJumpFalse
	opJump
	opHalt
)

type instruction struct {
	op      int
	operand int64
}

// execute runs a program over a small register file and an operand stack.
func execute(program []instruction, iterations int64) int64 {
	var stack [64]int64
	var registers [8]int64
	registers[0] = iterations
	top := 0
	pc := 0
	for {
		in := program[pc]
		pc++
		switch in.op {
		case opPush:
			stack[top] = in.operand
			top++
		case opLoad:
			stack[top] = registers[in.operand]
			top++
		case opStore:
			top--
			registers[in.operand] = stack[top]
		case opAdd:
			top--
			stack[top-1] += stack[top]
		case opSub:
			top--
			stack[top-1] -= stack[top]
		case opMul:
			top--
			stack[top-1] *= stack[top]
		case opXor:
			top--
			stack[top-1] ^= stack[top]
		case opLess:
			top--
			if stack[top-1] < stack[top] {
				stack[top-1] = 1
			} else {
				stack[top-1] = 0
			}
		case opJumpFalse:
			top--
			if stack[top] == 0 {
				pc = int(in.operand)
			}
		case opJump:
			pc = int(in.operand)
		case opHalt:
			return registers[1]
		}
	}
}

// counterLoop is `for i := 0; i < n; i++ { acc = acc*3 + i ^ acc }` in bytecode.
func counterLoop() []instruction {
	return []instruction{
		{opPush, 0}, {opStore, 2}, // i = 0
		{opPush, 1}, {opStore, 1}, // acc = 1
		// loop head at 4
		{opLoad, 2}, {opLoad, 0}, {opLess, 0}, {opJumpFalse, 21},
		{opLoad, 1}, {opPush, 3}, {opMul, 0}, {opLoad, 2}, {opAdd, 0},
		{opLoad, 1}, {opXor, 0}, {opStore, 1},
		{opLoad, 2}, {opPush, 1}, {opAdd, 0}, {opStore, 2},
		{opJump, 4},
		{opHalt, 0},
	}
}

func main() {
	report("control/spin-fixed-work", control)

	program := counterLoop()
	report("interp/bytecode-loop", func() {
		sink += uint64(execute(program, 800_000))
	})
}
''')

PROGRAMS['regexp'] = ('''// Command regexpbench times regexp matching, which is a second interpreter --
// the package compiles a pattern to a program and runs it over the input -- with
// a much larger dispatch than interpbench's.
package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)
''', '''
func main() {
	report("control/spin-fixed-work", control)

	var builder strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&builder, "2024-%02d-%02d host%d sshd[%d]: accepted key for user%d from 10.%d.%d.%d port %d\\n",
			i%12+1, i%28+1, i%7, 1000+i, i%50, i%256, (i*7)%256, (i*13)%256, 20000+i)
	}
	corpus := builder.String()

	address := regexp.MustCompile(`(\\d+)\\.(\\d+)\\.(\\d+)\\.(\\d+)`)
	entry := regexp.MustCompile(`^(\\d{4})-(\\d{2})-(\\d{2}) (\\w+) (\\w+)\\[(\\d+)\\]: (.*)$`)
	report("regexp/find-submatch", func() {
		for i := 0; i < 2; i++ {
			sink += uint64(len(address.FindAllStringSubmatch(corpus, -1)))
		}
	})
	report("regexp/anchored-lines", func() {
		lines := strings.Split(corpus, "\\n")
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
''')

PROGRAMS['json'] = ('''// Command jsonbench times an encoding/json round trip, which is reflection,
// interface dispatch and a hand-written scanner rather than arithmetic.
package main

import (
	"encoding/json"
	"fmt"
	"time"
)
''', '''
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
''')

PROGRAMS['sortmap'] = ('''// Command sortmapbench times sorting through a comparison callback and building
// and probing maps: indirect calls and the runtime's hash paths, neither of
// which is a loop the compiler can see all of.
package main

import (
	"fmt"
	"sort"
	"time"
)
''', '''
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
''')

PROGRAMS['flate'] = ('''// Command flatebench times a compress/flate round trip: table-driven loops over
// a window, with a match search that is all data-dependent branches.
package main

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"time"
)
''', '''
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
			buffer.WriteByte('\\n')
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
''')

PROGRAMS['text'] = ('''// Command textbench times number formatting and parsing and string building --
// strconv's digit loops, fmt's reflective dispatch, and utf8 decoding.
package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)
''', '''
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
''')


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    for name, (head, body) in PROGRAMS.items():
        path = os.path.join(here, name, 'main.go')
        with open(path, 'w') as f:
            f.write(head)
            f.write(HARNESS)
            f.write(body)
        print(path)


if __name__ == '__main__':
    main()
