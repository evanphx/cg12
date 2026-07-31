package main

// batchdiff answers the one question `goc compile-batch` has to answer: does
// compiling a program in a process that has already compiled other programs
// produce the same executable as compiling it alone?
//
// It compiles every named program twice -- once with a one-shot `goc` per
// program, once through a pool of `goc compile-batch` workers that each compile
// many -- and compares the two executables byte for byte. Anything that differs
// is either a leak between compiles in one process or a program whose compile is
// not deterministic to begin with.
//
// Telling those apart takes two steps, because bytes alone cannot do it: this
// compiler lays the same functions out at different addresses on each compile
// (RUNTIME_PLAN.md section 5.10), and on this corpus seventeen programs give a
// different image on every one of five compiles. So a differing program is asked
// first what actually moved -- see contentDigestOf -- and only if its *content*
// differs is it recompiled alone -repeats more times to see whether a solitary
// compile ever produces that content too.
//
// A third pass hands the same programs to the workers in the opposite order.
// Byte-identical output under two different groupings is much stronger evidence
// than one grouping: a leak that happens to be order-insensitive is possible but
// a leak that survives reversing every worker's history is not.
//
// -runtime takes the whole comma-separated pack set, not one pack, because with
// several packs a worker's history includes which of them it has already read
// and which program made it read them. That is the state this tool exists to
// catch crossing a program boundary, so it has to be present for the answer to
// mean anything.
//
// This is measurement, not a shipped tool.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type outcome struct {
	name     string
	digest   string
	duration time.Duration
	err      string
}

func main() {
	compiler := flag.String("goc", "", "path to a built goc binary")
	pack := flag.String("runtime", "", "comma-separated prebuilt runtime packs; empty compiles monolithically")
	work := flag.String("work", "", "scratch directory")
	workers := flag.Int("j", 16, "concurrent compiles, and batch workers")
	optimize := flag.Bool("O", false, "compile with -O")
	repeats := flag.Int("repeats", 8, "solo recompiles of each differing program, used to tell a leak from a nondeterministic compile")
	flag.Parse()
	programs := flag.Args()
	if *compiler == "" || *work == "" || len(programs) == 0 || *repeats < 1 {
		fmt.Fprintln(os.Stderr, "usage: batchdiff -goc goc -work dir [-runtime packs] [-O] [-j n] [-repeats n] program.go...")
		os.Exit(2)
	}

	fmt.Printf("programs=%d workers=%d pack=%q\n", len(programs), *workers, *pack)

	aloneStart := time.Now()
	alone := compileEachAlone(*compiler, *pack, *work, ".alone", *optimize, *workers, programs)
	aloneWall := time.Since(aloneStart)
	report("one-shot", alone, aloneWall)

	batchedStart := time.Now()
	batched := compileThroughBatch(*compiler, *pack, *work, ".batch", *optimize, *workers, programs)
	batchedWall := time.Since(batchedStart)
	report("batch", batched, batchedWall)

	reversedPrograms := make([]string, len(programs))
	for index, program := range programs {
		reversedPrograms[len(programs)-1-index] = program
	}
	reversedStart := time.Now()
	reversed := compileThroughBatch(*compiler, *pack, *work, ".reversed", *optimize, *workers, reversedPrograms)
	reversedWall := time.Since(reversedStart)
	report("batch-reversed", reversed, reversedWall)

	fmt.Printf("\nwall: one-shot=%.1fs batch=%.1fs (%.2fx) batch-reversed=%.1fs\n",
		aloneWall.Seconds(), batchedWall.Seconds(), aloneWall.Seconds()/batchedWall.Seconds(), reversedWall.Seconds())

	var aloneCPU, batchedCPU time.Duration
	for name, result := range alone {
		aloneCPU += result.duration
		batchedCPU += batched[name].duration
	}
	fmt.Printf("summed per-program compile wall: one-shot=%.1fs batch=%.1fs (%.1fs saved, %.1f%%)\n",
		aloneCPU.Seconds(), batchedCPU.Seconds(), (aloneCPU - batchedCPU).Seconds(),
		100*(aloneCPU-batchedCPU).Seconds()/aloneCPU.Seconds())

	differing := compare(programs, alone, batched, reversed)
	fmt.Printf("\nidentical=%d differing=%d\n", len(programs)-len(differing), len(differing))

	leaks := 0
	if len(differing) > 0 {
		fmt.Printf("\ntriaging the %d differing programs: what moved, and whether a solitary compile reproduces it\n", len(differing))
		solo := soloContentDigests(*compiler, *pack, *work, *optimize, *workers, *repeats, differing)
		for _, program := range differing {
			name := filepath.Base(program)
			aloneContent := contentDigestOf(filepath.Join(*work, name+".alone"))
			batchContent := contentDigestOf(filepath.Join(*work, name+".batch"))
			reversedContent := contentDigestOf(filepath.Join(*work, name+".reversed"))
			if aloneContent == batchContent && aloneContent == reversedContent {
				fmt.Printf("LAYOUT ONLY            %-46s same symbols, same sizes, same image size; only addresses moved\n", name)
				continue
			}
			seen := solo[name]
			seen[aloneContent] = true
			batchExplained := seen[batchContent]
			reversedExplained := seen[reversedContent]
			if batchExplained && reversedExplained {
				fmt.Printf("NONDETERMINISTIC ALONE %-46s content differs, but %d solitary compiles produce both batch results\n",
					name, *repeats+1)
				continue
			}
			leaks++
			fmt.Printf("LEAK                   %-46s alone=%s batch=%s reversed=%s; content differs and %s is not among the %d distinct contents %d solitary compiles produced\n",
				name, short(alone[name].digest), short(batched[name].digest), short(reversed[name].digest),
				unexplainedLabel(batchExplained, reversedExplained), len(seen), *repeats+1)
		}
		fmt.Printf("\nleaks=%d explained=%d\n", leaks, len(differing)-leaks)
	}

	// Bytes are the strong check but they cannot speak for a program whose
	// compile is already nondeterministic, and this corpus has some. So every
	// program is also run three ways and its behaviour compared: that is what the
	// matrix actually asserts, and it is the only evidence available where the
	// bytes legitimately differ.
	fmt.Printf("\nrunning all three builds of every program and comparing exit status and output\n")
	behaved := compareBehaviour(*work, programs, *workers)
	fmt.Printf("behaviour: identical=%d differing=%d\n", len(programs)-behaved, behaved)

	if leaks > 0 || behaved > 0 {
		os.Exit(1)
	}
	if len(differing) == 0 {
		fmt.Printf("\nIDENTICAL: all %d programs compile to the same bytes alone, in a batch, and in a reversed batch\n", len(programs))
	}
}

// compareBehaviour runs the three builds of every program and reports how many
// disagree on exit status or output.
//
// A first disagreement is not the answer. A handful of corpus programs print
// allocation and GC statistics, and those move with scheduling rather than with
// what compiled the program: measured 2026-07-31, one single executable of
// `bytes_grow_compare.go` produced three distinct outputs in 21 runs under the
// concurrency this function uses, and `bytes_grow_stats.go` two in 21 -- and the
// three builds of the latter were the same file, byte for byte. So a program
// whose three builds disagree is run repeatedly and reported only if some build's
// set of outputs never overlaps another's, which is what a program that was
// actually compiled differently would look like.
func compareBehaviour(work string, programs []string, workers int) int {
	const disagreementRepeats = 5

	type behaviour struct {
		name     string
		differed bool
		detail   string
	}
	results := make([]behaviour, len(programs))
	var group sync.WaitGroup
	slots := make(chan struct{}, workers)
	for index, program := range programs {
		group.Add(1)
		go func(index int, program string) {
			defer group.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			name := filepath.Base(program)
			alone := filepath.Join(work, name+".alone")
			batch := filepath.Join(work, name+".batch")
			reversed := filepath.Join(work, name+".reversed")
			aloneOutput, aloneCode := runBuild(alone)
			batchOutput, batchCode := runBuild(batch)
			reversedOutput, reversedCode := runBuild(reversed)
			if aloneCode == batchCode && aloneCode == reversedCode &&
				aloneOutput == batchOutput && aloneOutput == reversedOutput {
				results[index] = behaviour{name: name}
				return
			}

			aloneBehaviours := repeatedBehaviours(alone, disagreementRepeats)
			batchBehaviours := repeatedBehaviours(batch, disagreementRepeats)
			reversedBehaviours := repeatedBehaviours(reversed, disagreementRepeats)
			aloneBehaviours[behaviourKey(aloneOutput, aloneCode)] = true
			batchBehaviours[behaviourKey(batchOutput, batchCode)] = true
			reversedBehaviours[behaviourKey(reversedOutput, reversedCode)] = true
			if overlaps(aloneBehaviours, batchBehaviours) && overlaps(aloneBehaviours, reversedBehaviours) {
				results[index] = behaviour{name: name}
				return
			}
			results[index] = behaviour{
				name:     name,
				differed: true,
				detail: fmt.Sprintf("alone rc=%d batch rc=%d reversed rc=%d; %d/%d/%d distinct behaviours in %d runs each, and they do not overlap\n  alone   : %s\n  batch   : %s\n  reversed: %s",
					aloneCode, batchCode, reversedCode,
					len(aloneBehaviours), len(batchBehaviours), len(reversedBehaviours), disagreementRepeats+1,
					trim(aloneOutput), trim(batchOutput), trim(reversedOutput)),
			}
		}(index, program)
	}
	group.Wait()

	differing := 0
	for _, result := range results {
		if !result.differed {
			continue
		}
		differing++
		fmt.Printf("BEHAVIOUR DIFFERS      %-46s %s\n", result.name, result.detail)
	}
	return differing
}

// behaviourKey is one observation of what running a build does: its exit status
// and everything it printed.
func behaviourKey(output string, code int) string {
	return fmt.Sprintf("rc=%d\n%s", code, output)
}

// repeatedBehaviours runs one build several times and returns the distinct
// behaviours it showed.
func repeatedBehaviours(path string, repeats int) map[string]bool {
	behaviours := make(map[string]bool, repeats)
	for repeat := 0; repeat < repeats; repeat++ {
		output, code := runBuild(path)
		behaviours[behaviourKey(output, code)] = true
	}
	return behaviours
}

// overlaps reports whether two builds ever behaved the same way.
func overlaps(left, right map[string]bool) bool {
	for behaviour := range left {
		if right[behaviour] {
			return true
		}
	}
	return false
}

// runBuild executes one build under a deadline, because RUNTIME_PLAN 5.10
// records rare unexplained hangs and a comparison that stalls reports nothing.
func runBuild(path string) (string, int) {
	runContext, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	command := exec.CommandContext(runContext, path)
	command.Env = append(os.Environ(), "GOMAXPROCS=2")
	output, err := command.CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		code = -1
	}
	if runContext.Err() != nil {
		return string(output) + "\n[timed out]", -2
	}
	return string(output), code
}

// compare returns the programs whose three builds are not the same bytes.
//
// A program the compiler rejects all three ways is not a difference: this corpus
// is compiled by other suites too, and a program that does not build is their
// business. What matters here is a program that builds differently, including
// one that builds alone and fails in a batch.
func compare(programs []string, alone, batched, reversed map[string]outcome) []string {
	var differing []string
	rejected := 0
	for _, program := range programs {
		name := filepath.Base(program)
		aloneResult, batchResult, reversedResult := alone[name], batched[name], reversed[name]
		if aloneResult.err != "" && batchResult.err != "" && reversedResult.err != "" {
			rejected++
			continue
		}
		if aloneResult.err != "" || batchResult.err != "" || reversedResult.err != "" {
			fmt.Printf("BUILD DISAGREES        %-46s alone=%q batch=%q reversed=%q\n",
				name, trim(aloneResult.err), trim(batchResult.err), trim(reversedResult.err))
			differing = append(differing, program)
			continue
		}
		if aloneResult.digest == batchResult.digest && aloneResult.digest == reversedResult.digest {
			continue
		}
		differing = append(differing, program)
	}
	if rejected > 0 {
		fmt.Printf("%d program(s) the compiler rejects all three ways; not compared\n", rejected)
	}
	return differing
}

func report(label string, results map[string]outcome, wall time.Duration) {
	failed := 0
	for _, result := range results {
		if result.err != "" {
			failed++
		}
	}
	fmt.Printf("%-16s %d programs in %.1fs, %d failed\n", label, len(results), wall.Seconds(), failed)
}

// compileEachAlone runs one `goc` process per program, which is what the matrix
// does today and what the batch pass has to reproduce exactly.
func compileEachAlone(compiler, pack, work, suffix string, optimize bool, workers int, programs []string) map[string]outcome {
	results := make(map[string]outcome, len(programs))
	var mutex sync.Mutex
	var group sync.WaitGroup
	slots := make(chan struct{}, workers)
	for _, program := range programs {
		group.Add(1)
		go func(program string) {
			defer group.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			name := filepath.Base(program)
			output := filepath.Join(work, name+suffix)
			arguments := []string{"-o", output}
			if optimize {
				arguments = append(arguments, "-O")
			}
			if pack != "" {
				arguments = append(arguments, "-runtime", pack)
			}
			arguments = append(arguments, program)
			started := time.Now()
			combined, err := exec.Command(compiler, arguments...).CombinedOutput()
			result := outcome{name: name, duration: time.Since(started)}
			if err != nil {
				result.err = err.Error() + " " + string(combined)
			} else {
				result.digest = digestOf(output)
			}
			mutex.Lock()
			results[name] = result
			mutex.Unlock()
		}(program)
	}
	group.Wait()
	return results
}

// contentDigestOf identifies what an executable contains rather than where it
// put it: every defined symbol's name, size, kind and section, sorted, plus the
// image's total size.
//
// It exists because raw bytes cannot triage this compiler. goc's output is not
// reproducible (RUNTIME_PLAN.md section 5.10) and the cause that remains is
// ordering: 441 interface-call wrapper functions land in the module in a
// different order on each compile, so the same functions with the same code get
// different addresses. Two images that differ only that way have the same content
// digest. An image that gained, lost or resized a symbol does not, and that is
// what a compile contaminated by a previous compile would look like.
//
// This is necessary rather than sufficient -- two functions of equal size can
// hold different instructions -- so a matching content digest is reported as
// "layout only" and not as proof of equality.
func contentDigestOf(path string) string {
	file, err := elf.Open(path)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	defer file.Close()
	symbols, err := file.Symbols()
	if err != nil {
		return "no symbol table: " + err.Error()
	}
	described := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		if symbol.Section == elf.SHN_UNDEF {
			continue
		}
		described = append(described, fmt.Sprintf("%s %d %d %d", symbol.Name, symbol.Size, symbol.Info, symbol.Section))
	}
	sort.Strings(described)

	information, err := os.Stat(path)
	if err != nil {
		return "unstattable: " + err.Error()
	}
	digest := sha256.New()
	fmt.Fprintf(digest, "size=%d symbols=%d\n", information.Size(), len(described))
	for _, description := range described {
		fmt.Fprintln(digest, description)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// soloContentDigests compiles each program alone several times and returns the
// set of distinct contents each one produced.
//
// One repeat is not enough and reporting a leak on the strength of one is worse
// than reporting nothing. Measured on this corpus, seventeen of the programs that
// vary give a different *image* on every one of five compiles, so a repeat that
// happens to match proves only that two draws collided -- and a repeat that does
// not match proves nothing either. Comparing contents rather than images is what
// makes the question answerable at all: the ordering noise cancels, and what is
// left is whether a solitary compile ever produces what the batch produced.
func soloContentDigests(
	compiler, pack, work string,
	optimize bool,
	workers int,
	repeats int,
	programs []string,
) map[string]map[string]bool {
	digests := make(map[string]map[string]bool, len(programs))
	for _, program := range programs {
		digests[filepath.Base(program)] = map[string]bool{}
	}
	for repeat := 0; repeat < repeats; repeat++ {
		suffix := fmt.Sprintf(".solo%d", repeat)
		round := compileEachAlone(compiler, pack, work, suffix, optimize, workers, programs)
		for name, result := range round {
			if result.err != "" {
				continue
			}
			digests[name][contentDigestOf(filepath.Join(work, name+suffix))] = true
		}
	}
	return digests
}

// unexplainedLabel names which of the two batch groupings produced an executable
// no solitary compile did, which is the part of a leak report worth reading.
func unexplainedLabel(batchExplained, reversedExplained bool) string {
	switch {
	case !batchExplained && !reversedExplained:
		return "neither batch nor reversed"
	case !batchExplained:
		return "batch"
	default:
		return "reversed"
	}
}

// compileThroughBatch dispatches the programs across a pool of long-lived
// `goc compile-batch` workers, one program at a time per worker, so the grouping
// is decided by which worker happens to be free -- the same dynamic schedule the
// matrix harness uses.
func compileThroughBatch(compiler, pack, work, suffix string, optimize bool, workers int, programs []string) map[string]outcome {
	results := make(map[string]outcome, len(programs))
	var mutex sync.Mutex
	queue := make(chan string)
	go func() {
		for _, program := range programs {
			queue <- program
		}
		close(queue)
	}()

	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			arguments := []string{"compile-batch"}
			if optimize {
				arguments = append(arguments, "-O")
			}
			if pack != "" {
				arguments = append(arguments, "-runtime", pack)
			}
			command := exec.Command(compiler, arguments...)
			stdin, err := command.StdinPipe()
			if err != nil {
				panic(err)
			}
			stdout, err := command.StdoutPipe()
			if err != nil {
				panic(err)
			}
			command.Stderr = os.Stderr
			if err := command.Start(); err != nil {
				panic(err)
			}
			responses := bufio.NewScanner(stdout)
			responses.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

			for program := range queue {
				name := filepath.Base(program)
				output := filepath.Join(work, name+suffix)
				request, err := json.Marshal(map[string]string{"source": program, "output": output})
				if err != nil {
					panic(err)
				}
				if _, err := stdin.Write(append(request, '\n')); err != nil {
					panic(err)
				}
				if !responses.Scan() {
					panic(fmt.Sprintf("batch worker died compiling %s: %v", name, responses.Err()))
				}
				var response struct {
					Error   string  `json:"error"`
					Seconds float64 `json:"seconds"`
				}
				if err := json.Unmarshal(responses.Bytes(), &response); err != nil {
					panic(err)
				}
				result := outcome{name: name, duration: time.Duration(response.Seconds * float64(time.Second))}
				if response.Error != "" {
					result.err = response.Error
				} else {
					result.digest = digestOf(output)
				}
				mutex.Lock()
				results[name] = result
				mutex.Unlock()
			}
			stdin.Close()
			if err := command.Wait(); err != nil {
				panic(err)
			}
		}()
	}
	group.Wait()
	return results
}

func digestOf(path string) string {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func trim(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 160 {
		message = message[:160] + "..."
	}
	return message
}
