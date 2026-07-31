package main

// batchdiff answers the one question `goc compile-batch` has to answer: does
// compiling a program in a process that has already compiled other programs
// produce the same executable as compiling it alone?
//
// It compiles every named program twice -- once with a one-shot `goc` per
// program, once through a pool of `goc compile-batch` workers that each compile
// many -- and compares the two executables byte for byte. Anything that differs
// is either a leak between compiles in one process or a program whose compile is
// not deterministic to begin with, and the two are told apart by recompiling the
// differing program alone a second time.
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
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	flag.Parse()
	programs := flag.Args()
	if *compiler == "" || *work == "" || len(programs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: batchdiff -goc goc -work dir [-runtime pack] [-O] [-j n] program.go...")
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
		fmt.Printf("\nrecompiling each differing program alone a second time, to tell a leak from a compile that is not deterministic to begin with\n")
		second := compileEachAlone(*compiler, *pack, *work, ".alone2", *optimize, *workers, differing)
		for _, program := range differing {
			name := filepath.Base(program)
			if second[name].digest != alone[name].digest {
				fmt.Printf("NONDETERMINISTIC ALONE %-46s %s vs %s\n", name, short(alone[name].digest), short(second[name].digest))
				continue
			}
			leaks++
			fmt.Printf("LEAK                   %-46s alone=%s batch=%s reversed=%s\n",
				name, short(alone[name].digest), short(batched[name].digest), short(reversed[name].digest))
		}
		fmt.Printf("\nleaks=%d nondeterministic-alone=%d\n", leaks, len(differing)-leaks)
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
func compareBehaviour(work string, programs []string, workers int) int {
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
			aloneOutput, aloneCode := runBuild(filepath.Join(work, name+".alone"))
			batchOutput, batchCode := runBuild(filepath.Join(work, name+".batch"))
			reversedOutput, reversedCode := runBuild(filepath.Join(work, name+".reversed"))
			if aloneCode == batchCode && aloneCode == reversedCode &&
				aloneOutput == batchOutput && aloneOutput == reversedOutput {
				results[index] = behaviour{name: name}
				return
			}
			results[index] = behaviour{
				name:     name,
				differed: true,
				detail: fmt.Sprintf("alone rc=%d batch rc=%d reversed rc=%d\n  alone   : %s\n  batch   : %s\n  reversed: %s",
					aloneCode, batchCode, reversedCode, trim(aloneOutput), trim(batchOutput), trim(reversedOutput)),
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
