package main

// determinism answers one question about the whole corpus: does compiling the
// same program twice give the same executable?
//
// It compiles every named program -rounds times and reports, per program, how
// many distinct images came out. A program that gives one image in every round
// is reproducible; anything else is not, and RUNTIME_PLAN.md section 5.10
// records why that mattered enough to measure -- a build that is not
// reproducible cannot be compared byte for byte against another build, so every
// other check that wanted to do so had to work around it.
//
// A program that varies is also described by *content* -- every defined
// symbol's name, size, kind and section, sorted, plus the image size -- because
// the two failure modes look completely different under that lens. Functions
// laid out at different addresses keep one content digest across every round;
// functions compiled differently do not. The first is a layout residue, the
// second is a real difference in what was generated.
//
// Compiles go through `goc compile-batch` workers rather than one process per
// program because the corpus is 358 programs and the shared source world is
// worth roughly a quarter of a small program's compile. That is safe for this
// measurement: Go randomizes map iteration per range statement, not per process,
// so a worker that compiles a program twice has no more reason to agree with
// itself than two processes do. Section 20's verification establishes separately
// that a batch compile equals a solitary one.
//
// This is measurement, not a shipped tool.

import (
	"bufio"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// request and response mirror `goc compile-batch`'s line protocol.
type request struct {
	Source string `json:"source"`
	Output string `json:"output"`
}

type response struct {
	Source  string  `json:"source"`
	Output  string  `json:"output"`
	Error   string  `json:"error,omitempty"`
	Seconds float64 `json:"seconds"`
}

// build is what one round produced for one program.
type build struct {
	image   string
	content string
	err     string
}

func main() {
	compiler := flag.String("goc", "", "path to a built goc binary")
	pack := flag.String("runtime", "", "comma-separated prebuilt runtime packs; empty compiles monolithically")
	work := flag.String("work", "", "scratch directory")
	workers := flag.Int("j", 8, "batch workers, and therefore concurrent compiles")
	optimize := flag.Bool("O", false, "compile with -O")
	rounds := flag.Int("rounds", 3, "how many times to compile every program")
	flag.Parse()
	programs := flag.Args()
	if *compiler == "" || *work == "" || len(programs) == 0 || *rounds < 2 {
		fmt.Fprintln(os.Stderr, "usage: determinism -goc goc -work dir [-runtime packs] [-O] [-j n] [-rounds n] program.go...")
		os.Exit(2)
	}

	fmt.Printf("programs=%d rounds=%d workers=%d optimize=%v pack=%q\n",
		len(programs), *rounds, *workers, *optimize, *pack)

	perRound := make([]map[string]build, *rounds)
	for round := 0; round < *rounds; round++ {
		started := time.Now()
		perRound[round] = compileRound(*compiler, *pack, *work, fmt.Sprintf(".r%d", round), *optimize, *workers, programs)
		failed := 0
		for _, result := range perRound[round] {
			if result.err != "" {
				failed++
			}
		}
		fmt.Printf("round %d: %d programs in %.1fs, %d failed\n",
			round, len(programs), time.Since(started).Seconds(), failed)
	}

	var failing, imageVaries, contentVaries []string
	for _, program := range programs {
		name := filepath.Base(program)
		images := map[string]bool{}
		contents := map[string]bool{}
		broken := false
		for round := 0; round < *rounds; round++ {
			result := perRound[round][name]
			if result.err != "" {
				broken = true
				continue
			}
			images[result.image] = true
			contents[result.content] = true
		}
		switch {
		case broken:
			failing = append(failing, name)
		case len(contents) > 1:
			contentVaries = append(contentVaries, fmt.Sprintf("%-52s %d images, %d contents", name, len(images), len(contents)))
		case len(images) > 1:
			imageVaries = append(imageVaries, fmt.Sprintf("%-52s %d images, same content", name, len(images)))
		}
	}
	sort.Strings(failing)
	sort.Strings(imageVaries)
	sort.Strings(contentVaries)

	report := func(label string, items []string) {
		fmt.Printf("\n%s: %d\n", label, len(items))
		for _, item := range items {
			fmt.Printf("  %s\n", item)
		}
	}
	report("failed to compile", failing)
	report("content varies between rounds", contentVaries)
	report("image varies, content identical (layout only)", imageVaries)

	reproducible := len(programs) - len(failing) - len(imageVaries) - len(contentVaries)
	fmt.Printf("\nreproducible=%d varying=%d failed=%d of %d over %d rounds\n",
		reproducible, len(imageVaries)+len(contentVaries), len(failing), len(programs), *rounds)
	if len(imageVaries)+len(contentVaries) > 0 || len(failing) > 0 {
		os.Exit(1)
	}
}

// compileRound compiles every program once, through a pool of batch workers, and
// returns each one's image and content digests.
func compileRound(compiler, pack, work, suffix string, optimize bool, workers int, programs []string) map[string]build {
	results := make(map[string]build, len(programs))
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
			reader := bufio.NewReaderSize(stdout, 1<<20)
			encoder := json.NewEncoder(stdin)
			for program := range queue {
				name := filepath.Base(program)
				output := filepath.Join(work, name+suffix)
				if err := encoder.Encode(request{Source: program, Output: output}); err != nil {
					panic(err)
				}
				line, err := reader.ReadBytes('\n')
				if err != nil {
					panic(fmt.Errorf("read reply for %s: %w", name, err))
				}
				var reply response
				if err := json.Unmarshal(line, &reply); err != nil {
					panic(fmt.Errorf("decode reply for %s: %w", name, err))
				}
				result := build{err: reply.Error}
				if result.err == "" {
					result.image = imageDigestOf(output)
					result.content = contentDigestOf(output)
				}
				mutex.Lock()
				results[name] = result
				mutex.Unlock()
			}
			stdin.Close()
			io.Copy(io.Discard, stdout)
			command.Wait()
		}()
	}
	group.Wait()
	return results
}

func imageDigestOf(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "unreadable: " + err.Error()
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// contentDigestOf describes what the image holds rather than where it holds it:
// every defined symbol's name, size, kind and section, sorted, plus the image
// size. Two builds that differ only in the order functions were laid out have
// the same content digest, so this is what separates a layout residue from a
// genuine difference in generated code.
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
