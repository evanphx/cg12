package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/internal/runtimepack"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

// batchRequest is one program to compile, written as one JSON object on one line
// of the batch compiler's stdin.
type batchRequest struct {
	Source string `json:"source"`
	Output string `json:"output"`
}

// batchResponse is what the batch compiler writes back, one JSON object per line
// of stdout, in request order.
//
// Error is the whole failure: the compiler's message plus anything the C
// toolchain printed while linking this program. A batch worker never writes a
// program's diagnostics to its own stderr, because its stderr is shared by every
// program it compiles and could not be attributed afterwards.
type batchResponse struct {
	Source string `json:"source"`
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
	// Seconds is the wall clock of this program's compile and link, so a driver
	// can report per-program timings exactly as it does for one-shot compiles.
	Seconds float64 `json:"seconds"`
	// PeakRSSBytes is the worker's high-water mark *for the process*, not for
	// this program: rusage does not reset, and a worker that has already
	// compiled net/http keeps that peak. It is reported so a driver can still
	// bound memory, but it answers "how large did this worker get" rather than
	// "how large is this program".
	PeakRSSBytes uint64 `json:"peak_rss_bytes"`
}

// compileBatchCommand implements `goc compile-batch`: one process that compiles
// many programs, so that the work every compile does before it looks at the
// program is done once instead of once per program.
//
// What is shared is goc.sharedSourceWorld, the parsed and type-checked closure of
// the Go runtime. Measured on this box, building it costs 0.53 s of wall clock
// and 0.73 s of CPU, and compiling a trivial program with it already built costs
// 1.52 s; so a one-shot `goc` spends about a quarter of a small program's compile
// rediscovering a standard library it will discard on exit.
//
// # Why a stream on stdin rather than a manifest
//
// A manifest -- a fixed list of programs given to a process at startup -- is
// simpler, and it was rejected because it partitions the work statically. The
// matrix's compile schedule is dynamic on purpose: it dispatches longest-first
// through a shared worker pool so that the eleven programs that cost 125-167 s
// each start immediately, and it bounds how far compilation may run ahead of the
// run phase so that compiled-but-not-yet-run executables cannot fill a small
// /tmp. Both properties are properties of a *queue*, and a static partition
// destroys them: one worker handed several expensive programs would still be
// compiling when the rest of the machine had gone idle.
//
// A request stream keeps the queue. The driver holds a pool of workers, hands
// each the next program when it is free, and gets the world sharing for free
// because a worker outlives its programs.
//
// # Why the configuration is on the command line and not in each request
//
// Target, prebuilt runtime and -O select which files a package is built from and
// how it is type-checked -- they are exactly the axes of goc's source-world key.
// A worker that accepted them per request would build a second world on the
// first request that differed, silently doubling its memory, and the caller would
// have no way to see it had happened. Making them process-wide makes a worker a
// single build configuration by construction.
func compileBatchCommand(arguments []string, input io.Reader, output io.Writer, errorOutput io.Writer) int {
	flags := flag.NewFlagSet("goc compile-batch", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	optimize := flags.Bool("O", false, "optimize cg12 IR")
	prebuiltRuntime := flags.String("runtime", "", "link against the prebuilt runtime written by `goc build-runtime`")
	targetName := flags.String("target", defaultTargetName(), "arm64 | amd64")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(errorOutput, "usage: goc compile-batch [-O] [-target arch] [-runtime runtime.gocrt] < requests.jsonl")
		return 2
	}
	target, err := goc.ParseTarget(*targetName)
	if err != nil {
		fmt.Fprintf(errorOutput, "%v\n", err)
		return 1
	}

	var pack *runtimepack.Pack
	if *prebuiltRuntime != "" {
		pack, err = runtimepack.Read(*prebuiltRuntime)
		if err != nil {
			fmt.Fprintf(errorOutput, "goc: %v\n", err)
			return 1
		}
	}

	requests := bufio.NewScanner(input)
	// Requests are two short paths. The default 64 KiB line limit is already far
	// more than that, but it is set explicitly so that a long path fails as a
	// scanner error rather than by silently splitting one request into two.
	requests.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	responses := json.NewEncoder(output)
	for requests.Scan() {
		line := strings.TrimSpace(requests.Text())
		if line == "" {
			continue
		}
		var request batchRequest
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			fmt.Fprintf(errorOutput, "goc: compile-batch: malformed request: %v\n", err)
			return 1
		}
		response := compileBatchRequest(target, pack, *optimize, request)
		if err := responses.Encode(&response); err != nil {
			fmt.Fprintf(errorOutput, "goc: compile-batch: %v\n", err)
			return 1
		}
	}
	if err := requests.Err(); err != nil {
		fmt.Fprintf(errorOutput, "goc: compile-batch: %v\n", err)
		return 1
	}
	return 0
}

// compileBatchRequest compiles and links one program, turning every failure --
// including a panic in the compiler -- into a response rather than into an exit.
//
// The panic recovery is not decoration. A one-shot goc that panics loses one
// program; a worker that panics would lose every program still queued behind it,
// which would turn one compiler bug into an unreadable suite failure. Recovering
// keeps the blast radius at one program and puts the stack in that program's
// diagnostics, exactly where a one-shot compile would have put it.
func compileBatchRequest(target goc.Target, pack *runtimepack.Pack, optimize bool, request batchRequest) (response batchResponse) {
	started := time.Now()
	response = batchResponse{Source: request.Source, Output: request.Output}
	defer func() {
		if recovered := recover(); recovered != nil {
			response.Error = fmt.Sprintf("goc: panic: %v\n%s", recovered, debug.Stack())
		}
		response.Seconds = time.Since(started).Seconds()
		response.PeakRSSBytes = batchPeakRSSBytes()
	}()

	if request.Source == "" || request.Output == "" {
		response.Error = "goc: compile-batch: a request needs both a source and an output"
		return response
	}
	if err := compileBatchProgram(target, pack, optimize, request.Source, request.Output); err != nil {
		response.Error = err.Error()
	}
	return response
}

// compileBatchProgram is the batch equivalent of one `goc [-runtime pack] -o out
// in.go` invocation. It mirrors main's choice of compile path so that a batch
// build and a one-shot build of the same program are the same compile.
func compileBatchProgram(target goc.Target, pack *runtimepack.Pack, optimize bool, source, output string) error {
	contents, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	name := filepath.Base(source)
	// Diagnostics from the compiler and from cc are collected rather than
	// printed: this process is shared, so anything written to its stderr belongs
	// to no particular program.
	var diagnostics strings.Builder
	if pack != nil {
		err = linkAgainstReadPrebuiltRuntime(target, pack, name, contents, output, optimize, &diagnostics)
		return batchError(err, &diagnostics)
	}

	var module *ir.Module
	if target == goc.TargetARM64 {
		// Only arm64 can be given the full Go runtime; other targets get the
		// freestanding subset, exactly as the one-shot path does.
		module, err = goc.CompileExecutableFor(target, name, contents)
	} else {
		module, err = goc.CompileFor(target, name, contents)
	}
	if err != nil {
		return err
	}
	if optimize {
		opt.OptimizeModule(module)
	}
	return batchError(linkModule(target, module, output, &diagnostics), &diagnostics)
}

// batchError folds whatever the C toolchain printed into the returned error, so
// that a link failure carries the linker's message the way it does when cc's
// stderr is the terminal.
func batchError(err error, diagnostics *strings.Builder) error {
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(diagnostics.String())
	if message == "" {
		return err
	}
	return fmt.Errorf("%w\n%s", err, message)
}

// batchPeakRSSBytes reports this process's high-water resident set, or 0 when
// the platform will not say.
func batchPeakRSSBytes() uint64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	if usage.Maxrss <= 0 {
		return 0
	}
	return uint64(usage.Maxrss) * 1024
}
