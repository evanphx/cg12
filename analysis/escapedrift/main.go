// Command escapedrift reduces "concurrent compiles in one process change escape
// placement" to the smallest thing that shows it.
//
// The claim under test is that two goroutines compiling two programs in one
// process can make one of them place an allocation differently than it would
// have alone. The tool answers it three ways, in increasing order of how much
// they resemble the test suite:
//
//	knob   compile the victim alone with opt.EscapeSummaries at each setting,
//	       and diff the placements. This is the ceiling: nothing concurrency
//	       does can move a placement the knob cannot move.
//	pair   two goroutines, a handshake between them. One holds the knob off
//	       across a window; the other compiles the victim entirely inside that
//	       window. Deterministic -- no timing, no repetition.
//	race   two goroutines and no handshake. One replays what
//	       goc.TestEscapeSummaryCost does (six compiles, three with the knob
//	       off); the other compiles the victim in a loop and reports every
//	       round whose placement differs from the alone-run. This is the
//	       statistical form, kept only to show the deterministic one is the
//	       same bug.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

var (
	victimPath = flag.String("victim", "",
		"compile this file as the victim instead of the built-in program")
	flipperPath = flag.String("flipper", "",
		"compile this file in the other goroutine instead of the built-in program")
	mode   = flag.String("mode", "knob", "knob | diag | pair | race")
	rounds = flag.Int("rounds", 3, "victim compiles in race mode")
	dump   = flag.Bool("dump", false, "print every allocation decision in knob mode")
	spin   = flag.Bool("spin", false, "in race mode, flip the knob in a bare loop instead of compiling")
)

// victimSource is the smallest program whose placement the summary table
// decides. keep does not leak its parameter, so with the cross-function fact
// table on, &value stays in use's frame; with the table off, LowerHeapAllocations
// cannot tell what keep does with the pointer and sends the object to the heap.
const victimSource = `package main

type point struct{ x, y int }

//go:noinline
func keep(p *point) int { return p.x + p.y }

func use() int {
	p := &point{x: 1, y: 2}
	return keep(p)
}

func main() { println(use()) }
`

// flipperSource is what the other goroutine compiles. Nothing about it matters
// except that compiling it takes long enough to hold the window open.
const flipperSource = `package main

//go:noinline
func work(n int) *int { m := n * 3; return &m }

func main() {
	total := 0
	for i := 0; i < 8; i++ {
		total += *work(i)
	}
	println(total)
}
`

// placement is one allocation's decision, keyed the way a placement test reads
// it: the function it landed in, the source position, and the type.
type placement struct {
	key   string
	frame bool
}

func placements(module *ir.Module) []placement {
	out := make([]placement, 0, len(module.AllocDecisions))
	for _, decision := range module.AllocDecisions {
		key := fmt.Sprintf("%s %d:%d:%d %s %s",
			decision.Func, decision.Pos.File, decision.Pos.Line, decision.Pos.Col,
			decision.Allocator, decision.Type)
		out = append(out, placement{key: key, frame: decision.Placement == ir.AllocInFrame})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// diff counts how many placements moved between two runs, in each direction.
// toHeap is the conservative direction, toFrame the permissive one.
func diff(before, after []placement) (toHeap, toFrame int, firstHeap, firstFrame string) {
	index := make(map[string]bool, len(before))
	for _, p := range before {
		index[p.key] = p.frame
	}
	for _, p := range after {
		was, ok := index[p.key]
		if !ok || was == p.frame {
			continue
		}
		if was && !p.frame {
			toHeap++
			if firstHeap == "" {
				firstHeap = p.key
			}
			continue
		}
		toFrame++
		if firstFrame == "" {
			firstFrame = p.key
		}
	}
	return toHeap, toFrame, firstHeap, firstFrame
}

// program is one thing to compile: either a file, or a source string held here.
type program struct {
	name   string
	source []byte
	whole  bool // compile the runtime closure too
}

func chooseProgram(path, builtinName, builtinSource string) program {
	if path == "" {
		return program{name: builtinName, source: []byte(builtinSource)}
	}
	source, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading %s: %v\n", path, err)
		os.Exit(1)
	}
	return program{name: path, source: source, whole: true}
}

func mustCompile(p program) *ir.Module {
	var module *ir.Module
	var err error
	if p.whole {
		module, err = goc.CompileExecutable(p.name, p.source)
	} else {
		module, err = goc.Compile(p.name, p.source)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "compiling %s: %v\n", p.name, err)
		os.Exit(1)
	}
	return module
}

func report(label string, base, got []placement) {
	toHeap, toFrame, firstHeap, firstFrame := diff(base, got)
	fmt.Printf("%-28s %4d moved to the heap, %4d moved to a frame\n", label, toHeap, toFrame)
	if firstHeap != "" {
		fmt.Printf("%-28s   first to heap:  %s\n", "", firstHeap)
	}
	if firstFrame != "" {
		fmt.Printf("%-28s   first to frame: %s\n", "", firstFrame)
	}
}

func main() {
	flag.Parse()

	victim := chooseProgram(*victimPath, "victim.go", victimSource)
	flipper := chooseProgram(*flipperPath, "flipper.go", flipperSource)

	switch *mode {
	case "knob":
		fmt.Printf("victim %s, one goroutine, knob moved by hand\n", victim.name)
		on := placements(mustCompile(victim))
		opt.EscapeSummaries = false
		off := placements(mustCompile(victim))
		opt.EscapeSummaries = true
		again := placements(mustCompile(victim))
		fmt.Printf("%d allocation decisions\n", len(on))
		if *dump {
			for i := range on {
				fmt.Printf("  on  frame=%-5v %s\n", on[i].frame, on[i].key)
			}
			for i := range off {
				fmt.Printf("  off frame=%-5v %s\n", off[i].frame, off[i].key)
			}
		}
		report("summaries on -> off", on, off)
		report("summaries off -> on again", off, again)
		report("on -> on again (control)", on, again)

	case "diag":
		// The other package-level knob tests move: opt.SetEscapeDiagLevel. If
		// the -m level changed placement it would be a second mechanism, so it
		// is checked the same way.
		fmt.Printf("victim %s, escape diagnostic level moved by hand\n", victim.name)
		opt.SetEscapeDiagWriter(io.Discard)
		opt.SetEscapeDiagLevel(0)
		off := placements(mustCompile(victim))
		opt.SetEscapeDiagLevel(2)
		on := placements(mustCompile(victim))
		opt.SetEscapeDiagLevel(0)
		fmt.Printf("%d allocation decisions\n", len(off))
		report("-m=0 -> -m=2", off, on)

	case "pair":
		fmt.Printf("victim %s, two goroutines, handshake (default knob %v, held at %v)\n",
			victim.name, opt.EscapeSummaries, !opt.EscapeSummaries)
		alone := placements(mustCompile(victim))
		fmt.Printf("%d allocation decisions compiled alone\n", len(alone))

		open := make(chan struct{})
		done := make(chan struct{})
		var concurrent []placement
		var wait sync.WaitGroup
		wait.Add(2)
		// Goroutine A is goc.TestEscapeSummaryCost's knob window, and nothing
		// else: it moves a package-level variable in opt and puts it back.
		go func() {
			defer wait.Done()
			previous := opt.EscapeSummaries
			// Hold it at the opposite of whatever this process started with.
			// TestEscapeSummaryCost moves it both ways; which way perturbs a
			// concurrent compile is decided by the default, i.e. by
			// GOC_ESCAPE_SUMMARIES.
			opt.EscapeSummaries = !previous
			close(open)
			<-done
			opt.EscapeSummaries = previous
		}()
		// Goroutine B is any other test in the package. It compiles a program
		// and reads where the allocations went.
		go func() {
			defer wait.Done()
			<-open
			concurrent = placements(mustCompile(victim))
			close(done)
		}()
		wait.Wait()
		report("alone -> concurrent", alone, concurrent)

	case "race":
		fmt.Printf("victim %s, flipper %s, no handshake\n", victim.name, flipper.name)
		alone := placements(mustCompile(victim))
		fmt.Printf("%d allocation decisions compiled alone\n", len(alone))

		stop := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(1)
		// Goroutine A replays TestEscapeSummaryCost: compile the program three
		// times with the knob off, three times with it on, restoring after.
		go func() {
			defer wait.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if *spin {
					// No compile in this goroutine, so it shares no lock with
					// the one that is compiling. This is the shape the race
					// detector can see.
					opt.EscapeSummaries = !opt.EscapeSummaries
					continue
				}
				func() {
					previous := opt.EscapeSummaries
					opt.EscapeSummaries = false
					defer func() { opt.EscapeSummaries = previous }()
					for round := 0; round < 3; round++ {
						mustCompile(flipper)
					}
				}()
				func() {
					previous := opt.EscapeSummaries
					opt.EscapeSummaries = true
					defer func() { opt.EscapeSummaries = previous }()
					for round := 0; round < 3; round++ {
						mustCompile(flipper)
					}
				}()
			}
		}()
		perturbed := 0
		for round := 0; round < *rounds; round++ {
			got := placements(mustCompile(victim))
			toHeap, toFrame, _, _ := diff(alone, got)
			if toHeap != 0 || toFrame != 0 {
				perturbed++
			}
			report(fmt.Sprintf("round %d", round), alone, got)
		}
		close(stop)
		wait.Wait()
		fmt.Printf("%d of %d victim compiles were perturbed\n", perturbed, *rounds)

	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", *mode)
		os.Exit(2)
	}
}
