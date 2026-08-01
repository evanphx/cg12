package goc_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// updateFrameEscapeBaseline rewrites the accepted baseline from this run
// instead of comparing against it. Use it after a change that deliberately
// alters which frame addresses are published, and read the diff before
// committing it: every line is a place where the compiler's escape decision
// disagrees with the code it emitted.
var updateFrameEscapeBaseline = flag.Bool("update-frame-escape-baseline", false,
	"rewrite testdata/frame_escape_baseline.txt from this run")

const frameEscapeBaselinePath = "testdata/frame_escape_baseline.txt"

// TestFrameEscapeAudit compiles every corpus program and checks the finished
// IR against the escape decision that produced it: no allocation left in a
// frame may have its address stored anywhere that outlives the frame.
//
// The check is opt.FrameEscapes; this test is what makes it run. A wrong "does
// not escape" is silent at compile time and arrives much later as a collector
// fault in an unrelated goroutine, so the only way it gets caught early is if
// something compiles the corpus and looks. Nothing did: the defect this test
// was written for -- ccwork/escape-analysis's 2724ac7 -- passed its own branch
// runs and was found by a capability failing minutes into a GC.
//
// The baseline is a list of the publications this tree already makes, not a
// certificate that they are harmless. Several are known hazards: cg12 returns a
// string, an interface, an error or a complex128 by writing the address of a
// sixteen-byte frame slot into the caller's result area, and the caller reads
// it after this frame is gone (RUNTIME_PLAN.md section 5.15's residual). The
// test is a ratchet: it fails on a publication that is not already listed, and
// it fails on a listed publication that has gone away, so the file cannot drift
// away from what the compiler does.
func TestFrameEscapeAudit(t *testing.T) {
	programs, err := filepath.Glob("testdata/*.go")
	require.NoError(t, err)
	require.NotEmpty(t, programs)

	found := auditCorpusFrameEscapes(t, programs)

	if *updateFrameEscapeBaseline {
		writeFrameEscapeBaseline(t, found)
		t.Skip("baseline rewritten; rerun without -update-frame-escape-baseline to check it")
	}

	accepted := readFrameEscapeBaseline(t)

	var appeared []string
	for _, key := range sortedKeys(found) {
		if !accepted[key] {
			appeared = append(appeared, fmt.Sprintf("%s\n      first seen compiling: %s", key, found[key]))
		}
	}
	var vanished []string
	for key := range accepted {
		if _, ok := found[key]; !ok {
			vanished = append(vanished, key)
		}
	}
	sort.Strings(vanished)

	assert.Empty(t, appeared,
		"a frame address is published past its frame in a place %s does not list.\n"+
			"Each line is an escape decision that disagrees with the emitted IR: an allocation\n"+
			"the compiler kept in a frame, whose address it then stored somewhere that outlives\n"+
			"the frame. Fix the decision, or -- if the publication is correct -- explain why and\n"+
			"rerun with -update-frame-escape-baseline.\n  %s",
		frameEscapeBaselinePath, strings.Join(appeared, "\n  "))
	assert.Empty(t, vanished,
		"%s lists a frame-address publication the compiler no longer makes.\n"+
			"That is usually good news; rerun with -update-frame-escape-baseline to record it.\n  %s",
		frameEscapeBaselinePath, strings.Join(vanished, "\n  "))
}

// auditCorpusFrameEscapes compiles each program and returns every distinct
// finding key, mapped to one program that produced it. Compilation dominates
// the cost, so the programs run concurrently, bounded so a corpus-wide run does
// not multiply goc's several-hundred-megabyte peak by the core count.
func auditCorpusFrameEscapes(t *testing.T, programs []string) map[string]string {
	t.Helper()

	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}

	var mutex sync.Mutex
	found := make(map[string]string)
	var failures []string

	work := make(chan string)
	var working sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		working.Add(1)
		go func() {
			defer working.Done()
			for program := range work {
				source, err := os.ReadFile(program)
				if err != nil {
					mutex.Lock()
					failures = append(failures, fmt.Sprintf("%s: %v", program, err))
					mutex.Unlock()
					continue
				}
				module, err := goc.CompileExecutable(program, source)
				if err != nil {
					mutex.Lock()
					failures = append(failures, fmt.Sprintf("%s: %v", program, err))
					mutex.Unlock()
					continue
				}
				escapes := opt.FrameEscapes(module)
				mutex.Lock()
				for _, escape := range escapes {
					key := normalizeFrameEscapeKey(escape.Key())
					if _, seen := found[key]; !seen {
						found[key] = filepath.Base(program)
					}
				}
				mutex.Unlock()
			}
		}()
	}
	for _, program := range programs {
		work <- program
	}
	close(work)
	working.Wait()

	sort.Strings(failures)
	require.Empty(t, failures, "every corpus program must compile for the audit to mean anything")
	return found
}

// normalizeFrameEscapeKey strips this checkout's directory from the stdlib
// paths a finding carries, so the baseline is the same file on every machine.
func normalizeFrameEscapeKey(key string) string {
	root, err := os.Getwd()
	if err != nil {
		return key
	}
	return strings.ReplaceAll(key, filepath.Dir(root)+string(filepath.Separator), "")
}

func sortedKeys(found map[string]string) []string {
	keys := make([]string, 0, len(found))
	for key := range found {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func readFrameEscapeBaseline(t *testing.T) map[string]bool {
	t.Helper()

	contents, err := os.ReadFile(frameEscapeBaselinePath)
	require.NoError(t, err)

	accepted := make(map[string]bool)
	for _, line := range strings.Split(string(contents), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		accepted[line] = true
	}
	return accepted
}

func writeFrameEscapeBaseline(t *testing.T, found map[string]string) {
	t.Helper()

	var builder strings.Builder
	builder.WriteString(frameEscapeBaselineHeader)
	for _, key := range sortedKeys(found) {
		builder.WriteString(key)
		builder.WriteString("\n")
	}
	require.NoError(t, os.WriteFile(frameEscapeBaselinePath, []byte(builder.String()), 0o644))
}

const frameEscapeBaselineHeader = `# Frame addresses this tree publishes past their frame, one per line, as
# opt.FrameEscapes reports them: source position, function, how the address
# left the frame, and what received it. Temporary numbers are deliberately not
# part of a line, so an unrelated change that emits one more instruction does
# not rewrite the file.
#
# This is a record of what the compiler does, not a list of things that are
# correct. TestFrameEscapeAudit fails on any publication not listed here and on
# any listed publication that has gone away, so the file tracks the compiler
# rather than drifting from it. Regenerate with
#
#     go test ./goc -run TestFrameEscapeAudit -update-frame-escape-baseline
#
# and read the diff: every added line is an allocation the compiler decided
# does not escape, whose address it then stored somewhere that outlives the
# frame.
`
