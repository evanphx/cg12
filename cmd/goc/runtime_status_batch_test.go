package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var runtimeStatusBatchCompile = flag.Bool(
	"runtime-status-batch-compile",
	true,
	"compile capability programs through long-lived `goc compile-batch` workers, so the parsed and type-checked runtime is shared by every program a worker compiles",
)

// runtimeCapabilityBatchPool is a pool of long-lived `goc compile-batch`
// processes, one program at a time each.
//
// It exists because the compile the matrix repeats 338 times begins by parsing
// and type-checking the Go runtime's source closure, and goc caches that per
// process. Measured on this box, a worker pays 0.53 s of wall clock and 0.73 s
// of CPU to build that world and then every later program it compiles skips it,
// so a pool of W workers pays it W times instead of 338.
//
// It amortizes the prebuilt runtimes as well. The matrix offers seven packs and
// every one-shot compile parsed all seven manifests to choose between them (§19,
// "what is not done"); a worker parses them once and reads each pack's objects
// once, the first time one of its programs picks that pack.
//
// A worker is a compile slot, not a partition of the work: the dispatcher hands
// each free worker the next program, exactly as it handed each free slot a new
// process before, so longest-first dispatch and the look-ahead bound are
// unchanged. That is the whole reason the batch compiler speaks a request stream
// rather than taking a list of files.
type runtimeCapabilityBatchPool struct {
	compiler string
	// prebuiltRuntime is goc's -runtime value: the whole comma-separated set of
	// packs, not one pack. Every worker is offered all of them and each program
	// still takes the richest its own closure allows.
	prebuiltRuntime string
	optimize        bool

	// slots bounds how many workers can exist, so a caller that dispatches more
	// concurrently than the pool was sized for waits rather than forking an
	// unbounded number of compilers.
	slots chan struct{}
	// free holds live idle workers. A worker that dies is not returned to it,
	// so the next acquire starts a replacement and one dead worker costs one
	// program rather than the rest of the run.
	free chan *runtimeCapabilityBatchWorker

	mutex   sync.Mutex
	started []*runtimeCapabilityBatchWorker
}

// runtimeCapabilityBatchWorker is one `goc compile-batch` process.
type runtimeCapabilityBatchWorker struct {
	command   *exec.Cmd
	requests  io.WriteCloser
	responses *bufio.Scanner

	// diagnostics collects anything the worker writes to its stderr. A batch
	// worker attributes each program's own diagnostics to that program in its
	// response, so whatever lands here is process-level -- a crash, or the Go
	// runtime's own complaint -- and belongs to whichever program was in flight.
	diagnostics      strings.Builder
	diagnosticsMutex sync.Mutex
	drained          sync.WaitGroup
	// stopped guards the shutdown, because a worker that died mid-program is
	// stopped by the compile that lost it and again when the pool shuts down.
	stopped sync.Once
}

func newRuntimeCapabilityBatchPool(compiler, prebuiltRuntime string, optimize bool, workers int) *runtimeCapabilityBatchPool {
	return &runtimeCapabilityBatchPool{
		compiler:        compiler,
		prebuiltRuntime: prebuiltRuntime,
		optimize:        optimize,
		slots:           make(chan struct{}, workers),
		free:            make(chan *runtimeCapabilityBatchWorker, workers),
	}
}

// compile compiles one program on some worker and returns the same shape of
// result a one-shot `goc` invocation returns.
func (pool *runtimeCapabilityBatchPool) compile(source, executable string) runtimeCapabilityCompilation {
	pool.slots <- struct{}{}
	defer func() { <-pool.slots }()

	worker, err := pool.acquire()
	if err != nil {
		return runtimeCapabilityCompilation{
			executable: executable,
			output:     err.Error(),
			err:        fmt.Errorf("start a batch compiler: %w", err),
		}
	}

	started := time.Now()
	response, requestErr := worker.compile(source, executable)
	if requestErr != nil {
		// The worker is gone or out of step; it is not reused. The program is
		// reported as a failed compile, which is what a one-shot goc that died
		// would have reported too.
		worker.stop()
		return runtimeCapabilityCompilation{
			executable: executable,
			output:     worker.takeDiagnostics(),
			err:        requestErr,
			duration:   time.Since(started),
		}
	}
	compilation := runtimeCapabilityCompilation{
		executable: executable,
		duration:   time.Duration(response.Seconds * float64(time.Second)),
		peakRSS:    response.PeakRSSBytes,
	}
	if response.Error != "" {
		compilation.output = response.Error + worker.takeDiagnostics()
		compilation.err = fmt.Errorf("compile failed")
	}
	// The worker goes back to the pool only after its diagnostics have been
	// taken. Returning it first would let the next program start writing to the
	// same stderr buffer before this one had read it, and a linker complaint
	// would be attributed to whichever program happened to win the race.
	pool.free <- worker
	return compilation
}

// acquire returns a live idle worker, starting one if the pool has none.
func (pool *runtimeCapabilityBatchPool) acquire() (*runtimeCapabilityBatchWorker, error) {
	select {
	case worker := <-pool.free:
		return worker, nil
	default:
	}

	arguments := []string{"compile-batch"}
	if pool.optimize {
		arguments = append(arguments, "-O")
	}
	if pool.prebuiltRuntime != "" {
		arguments = append(arguments, "-runtime", pool.prebuiltRuntime)
	}
	worker, err := startRuntimeCapabilityBatchWorker(pool.compiler, arguments)
	if err != nil {
		return nil, err
	}
	pool.mutex.Lock()
	pool.started = append(pool.started, worker)
	pool.mutex.Unlock()
	return worker, nil
}

// stop shuts every worker down. It is called once the compile queue has drained,
// so no worker is in the middle of a program.
func (pool *runtimeCapabilityBatchPool) stop() {
	pool.mutex.Lock()
	workers := pool.started
	pool.started = nil
	pool.mutex.Unlock()

	for _, worker := range workers {
		worker.stop()
	}
}

func startRuntimeCapabilityBatchWorker(compiler string, arguments []string) (*runtimeCapabilityBatchWorker, error) {
	command := exec.Command(compiler, arguments...)
	requests, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	responses, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errorOutput, err := command.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}

	worker := &runtimeCapabilityBatchWorker{
		command:  command,
		requests: requests,
	}
	scanner := bufio.NewScanner(responses)
	// A response carries the compiler's whole error message, which for a failed
	// compile can be long, so the line limit is generous. The default 64 KiB
	// would truncate one into a decode failure and lose the diagnosis.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	worker.responses = scanner

	// stderr is drained continuously rather than at the end, because a worker
	// that filled its stderr pipe would block forever with nobody reading it.
	worker.drained.Add(1)
	go func() {
		defer worker.drained.Done()
		reader := bufio.NewReader(errorOutput)
		buffer := make([]byte, 4096)
		for {
			read, err := reader.Read(buffer)
			if read > 0 {
				worker.diagnosticsMutex.Lock()
				worker.diagnostics.Write(buffer[:read])
				worker.diagnosticsMutex.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return worker, nil
}

func (worker *runtimeCapabilityBatchWorker) compile(source, executable string) (batchResponse, error) {
	request, err := json.Marshal(batchRequest{Source: source, Output: executable})
	if err != nil {
		return batchResponse{}, err
	}
	if _, err := worker.requests.Write(append(request, '\n')); err != nil {
		return batchResponse{}, fmt.Errorf("send %s to a batch compiler: %w", source, err)
	}
	if !worker.responses.Scan() {
		if err := worker.responses.Err(); err != nil {
			return batchResponse{}, fmt.Errorf("read the batch compiler's reply for %s: %w", source, err)
		}
		return batchResponse{}, fmt.Errorf("the batch compiler exited while compiling %s", source)
	}
	var response batchResponse
	if err := json.Unmarshal(worker.responses.Bytes(), &response); err != nil {
		return batchResponse{}, fmt.Errorf("decode the batch compiler's reply for %s: %w", source, err)
	}
	return response, nil
}

// takeDiagnostics returns what the worker has written to stderr so far and
// forgets it, so the next program does not inherit it.
func (worker *runtimeCapabilityBatchWorker) takeDiagnostics() string {
	worker.diagnosticsMutex.Lock()
	defer worker.diagnosticsMutex.Unlock()
	collected := worker.diagnostics.String()
	worker.diagnostics.Reset()
	return collected
}

func (worker *runtimeCapabilityBatchWorker) stop() {
	worker.stopped.Do(func() {
		worker.requests.Close()
		worker.drained.Wait()
		worker.command.Wait()
	})
}
