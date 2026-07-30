package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// The programs are deliberately small and varied rather than large: what is
// being tested is whether one compile can disturb the next, and a feature the
// compiler handles differently -- a closure, a defer, an interface, a map -- is
// a better probe for that than a big program that is mostly the same code paths
// again.
var batchProbePrograms = map[string]string{
	"batch_println.go": `package main

func main() { println("batch println") }
`,
	"batch_closure.go": `package main

func adder() func(int) int {
	total := 0
	return func(value int) int {
		total += value
		return total
	}
}

func main() {
	add := adder()
	add(2)
	println(add(3))
}
`,
	"batch_defer_interface.go": `package main

type shape interface{ area() int }

type square struct{ side int }

func (s square) area() int { return s.side * s.side }

func report() { println("done") }

func main() {
	defer report()
	var value shape = square{side: 5}
	println(value.area())
}
`,
	"batch_map_slice.go": `package main

func main() {
	counts := map[string]int{"a": 1}
	counts["b"] = 2
	values := make([]int, 0, 4)
	for _, count := range counts {
		values = append(values, count)
	}
	println(len(values), counts["b"])
}
`,
}

// TestBatchCompilesMatchOneShotCompiles is the test that decides whether
// `goc compile-batch` is safe to use.
//
// One process compiling many programs is worth about a quarter of a small
// program's compile, and it is worth nothing at all if a compile can be
// influenced by the compiles before it: a program miscompiled according to what
// its worker happened to see earlier is the worst failure this repository can
// produce, because nothing about the program's own source explains it.
//
// So each program is compiled four ways -- alone twice, in a batch, and in a
// batch that sees the programs in the opposite order -- and the batch builds
// have to be the same bytes as the solitary one.
//
// The second solitary compile is not redundant. Some programs in this corpus do
// not compile deterministically even alone (RUNTIME_PLAN 5.10's backend
// residue), and for those, byte equality is not a question the test can ask. It
// asks it of the rest and says how many it asked, so the test cannot quietly
// become vacuous.
func TestBatchCompilesMatchOneShotCompiles(t *testing.T) {
	compiler, pack, directory := batchTestEnvironment(t)
	sources := writeBatchProbePrograms(t, directory)

	firstAlone := compileEachAloneForTest(t, compiler, pack, directory, ".alone1", sources)
	secondAlone := compileEachAloneForTest(t, compiler, pack, directory, ".alone2", sources)
	forward := compileBatchForTest(t, compiler, pack, directory, ".forward", sources)
	reversed := compileBatchForTest(t, compiler, pack, directory, ".reversed", reverseStrings(sources))

	compared := 0
	for _, source := range sources {
		name := filepath.Base(source)
		if !bytes.Equal(firstAlone[name], secondAlone[name]) {
			t.Logf("%s does not compile deterministically on its own, so its bytes cannot be compared", name)
			continue
		}
		compared++
		require.Equal(t, firstAlone[name], forward[name],
			"%s compiled in a batch differs from %s compiled alone", name, name)
		require.Equal(t, firstAlone[name], reversed[name],
			"%s compiled in a reversed batch differs from %s compiled alone", name, name)
	}
	require.GreaterOrEqual(t, compared, 2,
		"too few programs compiled deterministically alone for this test to mean anything")
}

// TestBatchCompilerSurvivesAProgramItCannotCompile checks the property that
// makes a shared worker acceptable at all: one bad program costs one program.
//
// A one-shot goc that rejects a program exits, and the next program gets a fresh
// process. A worker has no fresh process to offer, so it has to reject the
// program, keep the failure attributed to it, and go on to compile the next one
// correctly.
func TestBatchCompilerSurvivesAProgramItCannotCompile(t *testing.T) {
	compiler, pack, directory := batchTestEnvironment(t)

	broken := filepath.Join(directory, "batch_broken.go")
	require.NoError(t, os.WriteFile(broken, []byte("package main\n\nfunc main() { thisIsNotDefined() }\n"), 0o644))
	working := filepath.Join(directory, "batch_println.go")
	require.NoError(t, os.WriteFile(working, []byte(batchProbePrograms["batch_println.go"]), 0o644))

	responses := runBatchCompiler(t, compiler, pack, []batchRequest{
		{Source: broken, Output: filepath.Join(directory, "broken.bin")},
		{Source: working, Output: filepath.Join(directory, "working.bin")},
	})
	require.Len(t, responses, 2)

	require.NotEmpty(t, responses[0].Error, "a program that does not type-check should be reported as an error")
	require.Contains(t, responses[0].Error, "thisIsNotDefined")
	require.Equal(t, broken, responses[0].Source)

	require.Empty(t, responses[1].Error, "the program after a failed one should still compile")
	output, err := exec.Command(filepath.Join(directory, "working.bin")).CombinedOutput()
	require.NoError(t, err)
	require.Contains(t, string(output), "batch println")
}

// TestBatchCompilerSharesItsWorldAcrossPrograms is the direct evidence that the
// mode does what it exists to do.
//
// The same program is compiled three times in one worker. The first pays for
// parsing and type-checking the Go runtime's source closure and the rest do not,
// which is measured on this box as 2.05 s against 1.50 s -- a margin of about
// 27%, so the assertion is a comfortable inequality rather than a threshold. If
// this ever fails, the world is no longer being shared and the mode is only
// saving process startup.
func TestBatchCompilerSharesItsWorldAcrossPrograms(t *testing.T) {
	compiler, pack, directory := batchTestEnvironment(t)

	source := filepath.Join(directory, "batch_println.go")
	require.NoError(t, os.WriteFile(source, []byte(batchProbePrograms["batch_println.go"]), 0o644))

	requests := make([]batchRequest, 0, 3)
	for index := 0; index < 3; index++ {
		requests = append(requests, batchRequest{
			Source: source,
			Output: filepath.Join(directory, fmt.Sprintf("shared%d.bin", index)),
		})
	}
	responses := runBatchCompiler(t, compiler, pack, requests)
	require.Len(t, responses, 3)
	for _, response := range responses {
		require.Empty(t, response.Error)
	}

	t.Logf("compiles of the same program in one worker: %.2fs %.2fs %.2fs",
		responses[0].Seconds, responses[1].Seconds, responses[2].Seconds)
	require.Less(t, responses[1].Seconds, responses[0].Seconds,
		"the second compile in a worker should skip building the source world")
	require.Less(t, responses[2].Seconds, responses[0].Seconds,
		"the third compile in a worker should skip building the source world")
}

// batchTestEnvironment builds the compiler, writes the shared prebuilt runtime
// to a file the command line can name, and returns a scratch directory.
func batchTestEnvironment(t *testing.T) (compiler, pack, directory string) {
	t.Helper()
	if runtime.GOARCH != "arm64" {
		t.Skip("the batch compiler shares the Go runtime's source world, which only arm64 compiles")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc is required to link")
	}

	directory = t.TempDir()
	pack = filepath.Join(directory, "runtime.gocrt")
	require.NoError(t, sharedPrebuiltRuntime(t).Write(pack))
	return sharedGOCBinary(t), pack, directory
}

func writeBatchProbePrograms(t *testing.T, directory string) []string {
	t.Helper()

	names := make([]string, 0, len(batchProbePrograms))
	for name := range batchProbePrograms {
		names = append(names, name)
	}
	// A map iterates in a random order, and the test compares one fixed order
	// against its reverse, so the order is pinned here.
	sort.Strings(names)

	sources := make([]string, 0, len(names))
	for _, name := range names {
		path := filepath.Join(directory, name)
		require.NoError(t, os.WriteFile(path, []byte(batchProbePrograms[name]), 0o644))
		sources = append(sources, path)
	}
	return sources
}

// compileEachAloneForTest compiles every program with a `goc` process of its
// own, which is what the matrix did before batching, and returns the bytes.
func compileEachAloneForTest(t *testing.T, compiler, pack, directory, suffix string, sources []string) map[string][]byte {
	t.Helper()

	built := make(map[string][]byte, len(sources))
	var mutex sync.Mutex
	var group sync.WaitGroup
	for _, source := range sources {
		group.Add(1)
		go func(source string) {
			defer group.Done()
			output := filepath.Join(directory, filepath.Base(source)+suffix)
			combined, err := exec.Command(compiler, "-runtime", pack, "-o", output, source).CombinedOutput()
			if err != nil {
				mutex.Lock()
				built[filepath.Base(source)] = []byte(fmt.Sprintf("compile failed: %v\n%s", err, combined))
				mutex.Unlock()
				return
			}
			contents, readErr := os.ReadFile(output)
			mutex.Lock()
			if readErr != nil {
				built[filepath.Base(source)] = []byte("unreadable: " + readErr.Error())
			} else {
				built[filepath.Base(source)] = contents
			}
			mutex.Unlock()
		}(source)
	}
	group.Wait()
	return built
}

// compileBatchForTest compiles every program in one worker, in the order given.
func compileBatchForTest(t *testing.T, compiler, pack, directory, suffix string, sources []string) map[string][]byte {
	t.Helper()

	requests := make([]batchRequest, 0, len(sources))
	for _, source := range sources {
		requests = append(requests, batchRequest{
			Source: source,
			Output: filepath.Join(directory, filepath.Base(source)+suffix),
		})
	}
	responses := runBatchCompiler(t, compiler, pack, requests)
	require.Len(t, responses, len(requests))

	built := make(map[string][]byte, len(responses))
	for _, response := range responses {
		require.Empty(t, response.Error, "compiling %s in a batch", response.Source)
		contents, err := os.ReadFile(response.Output)
		require.NoError(t, err)
		built[filepath.Base(response.Source)] = contents
	}
	return built
}

// runBatchCompiler drives one `goc compile-batch` process through a list of
// requests and returns the responses in order.
func runBatchCompiler(t *testing.T, compiler, pack string, requests []batchRequest) []batchResponse {
	t.Helper()

	command := exec.Command(compiler, "compile-batch", "-runtime", pack)
	stdin, err := command.StdinPipe()
	require.NoError(t, err)
	stdout, err := command.StdoutPipe()
	require.NoError(t, err)
	var diagnostics diagnosticsBuffer
	command.Stderr = &diagnostics
	require.NoError(t, command.Start())

	go func() {
		defer stdin.Close()
		for _, request := range requests {
			encoded, err := json.Marshal(request)
			if err != nil {
				return
			}
			if _, err := stdin.Write(append(encoded, '\n')); err != nil {
				return
			}
		}
	}()

	responses := make([]batchResponse, 0, len(requests))
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var response batchResponse
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &response), "reply %q", scanner.Text())
		responses = append(responses, response)
	}
	require.NoError(t, scanner.Err())
	require.NoError(t, command.Wait(), "batch compiler stderr:\n%s", diagnostics.String())
	return responses
}

// diagnosticsBuffer collects a child's stderr. exec writes to it from its own
// goroutine, so it carries a mutex of its own.
type diagnosticsBuffer struct {
	mutex    sync.Mutex
	contents []byte
}

func (buffer *diagnosticsBuffer) Write(chunk []byte) (int, error) {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	buffer.contents = append(buffer.contents, chunk...)
	return len(chunk), nil
}

func (buffer *diagnosticsBuffer) String() string {
	buffer.mutex.Lock()
	defer buffer.mutex.Unlock()
	return string(buffer.contents)
}

func reverseStrings(values []string) []string {
	reversed := make([]string, len(values))
	for index, value := range values {
		reversed[len(values)-1-index] = value
	}
	return reversed
}
