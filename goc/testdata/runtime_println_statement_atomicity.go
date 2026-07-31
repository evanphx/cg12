package main

import (
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

// runtime/print.go states the requirement outright: "The compiler emits calls
// to printlock and printunlock around the multiple calls that implement a
// single Go print or println statement." cg12 emitted neither, so one print
// statement was a run of unsynchronized writes to file descriptor 2 and two
// threads printing at once interleaved inside a line. Measured before the fix,
// eight goroutines at GOMAXPROCS 4 corrupted about 3000 of 3200 lines.
//
// This matters well beyond user code: every runtime diagnostic -- every
// traceback, every GODEBUG line, every fatal error -- is written by these same
// routines, and runtime.minhexdigits is documented as protected by that lock.
//
// The reducer captures file descriptor 2 into a temporary file, so the check is
// on the bytes the program actually wrote rather than on a terminal.
const (
	workers = 8
	rounds  = 300
)

func main() {
	runtime.GOMAXPROCS(4)

	file, err := os.CreateTemp("", "goc-println-atomicity")
	if err != nil {
		panic(err)
	}
	defer os.Remove(file.Name())

	saved, err := syscall.Dup(2)
	if err != nil {
		panic(err)
	}
	if err := syscall.Dup3(int(file.Fd()), 2, 0); err != nil {
		panic(err)
	}

	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(id int) {
			defer wait.Done()
			for round := 0; round < rounds; round++ {
				println("worker", id, "round", round, "tail", 1, 2, 3, 4, 5, 6, 7, 8)
			}
		}(worker)
	}
	wait.Wait()

	if err := syscall.Dup3(saved, 2, 0); err != nil {
		panic(err)
	}
	if err := syscall.Close(saved); err != nil {
		panic(err)
	}

	written, err := os.ReadFile(file.Name())
	if err != nil {
		panic(err)
	}
	file.Close()

	lines := strings.Split(strings.TrimSuffix(string(written), "\n"), "\n")
	if len(lines) != workers*rounds {
		panic("println wrote " + itoa(len(lines)) + " lines, want " + itoa(workers*rounds))
	}

	seen := make(map[string]bool, workers*rounds)
	for _, line := range lines {
		if strings.Count(line, "worker") != 1 || strings.Count(line, "tail") != 1 {
			panic("two print statements interleaved inside one line: " + line)
		}
		if !strings.HasPrefix(line, "worker ") || !strings.HasSuffix(line, " tail 1 2 3 4 5 6 7 8") {
			panic("println wrote a malformed line: " + line)
		}
		if seen[line] {
			panic("println wrote a duplicate line: " + line)
		}
		seen[line] = true
	}

	println("println-statement-atomicity ok")
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
