package goc_test

import (
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
	"github.com/stretchr/testify/require"
)

// corpusAudit is everything the whole-corpus audits extract from one pass over
// testdata/*.go: which frame addresses the tree publishes past their frame, and
// where every allocation lands.
//
// Both are maps from a finding's stable key to the base name of one program that
// produced it, so a failure can say where to look. Which program that is, when
// several produce the same key, depends on the order the workers finish and is
// not reproducible; it is a hint in a diagnostic, and no baseline records it.
// The keys themselves are the reproducible part.
type corpusAudit struct {
	// programs is the number of corpus programs compiled.
	programs int
	// frameEscapes is opt.FrameEscapes's findings. See TestFrameEscapeAudit.
	frameEscapes map[string]string
	// allocations is opt.AllocationCensus's records. See TestAllocationCensus.
	allocations map[string]string
	// failures is every program that did not compile; either audit is
	// meaningless if this is not empty.
	failures []string
}

var (
	corpusAuditOnce   sync.Once
	corpusAuditResult *corpusAudit
)

// auditCorpus compiles every corpus program once and returns what the audits
// found. Compiling the corpus takes minutes and dominates everything else these
// tests do, so the two audits that need it share a single pass rather than each
// paying for their own: the second test to ask gets the first one's result.
//
// Sharing is safe because neither audit changes anything -- both read a module
// the pass compiled and throw it away -- and because the result is keyed by
// nothing but the corpus, which does not vary between tests.
func auditCorpus(t *testing.T) *corpusAudit {
	t.Helper()

	corpusAuditOnce.Do(func() {
		programs, err := filepath.Glob("testdata/*.go")
		if err != nil {
			corpusAuditResult = &corpusAudit{failures: []string{err.Error()}}
			return
		}
		sort.Strings(programs)
		corpusAuditResult = compileCorpusForAudits(programs)
	})

	audit := corpusAuditResult
	require.NotEmpty(t, audit.programs, "the corpus must not be empty for an audit to mean anything")
	require.Empty(t, audit.failures, "every corpus program must compile for the audit to mean anything")
	return audit
}

// compileCorpusForAudits runs the audits over each program. Compilation
// dominates the cost, so the programs run concurrently, bounded so a
// corpus-wide run does not multiply goc's several-hundred-megabyte peak by the
// core count.
func compileCorpusForAudits(programs []string) *corpusAudit {
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}

	audit := &corpusAudit{
		programs:     len(programs),
		frameEscapes: make(map[string]string),
		allocations:  make(map[string]string),
	}

	var mutex sync.Mutex
	note := func(into map[string]string, key, program string) {
		if _, seen := into[key]; !seen {
			into[key] = program
		}
	}

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
					audit.failures = append(audit.failures, fmt.Sprintf("%s: %v", program, err))
					mutex.Unlock()
					continue
				}
				module, err := goc.CompileExecutable(program, source)
				if err != nil {
					mutex.Lock()
					audit.failures = append(audit.failures, fmt.Sprintf("%s: %v", program, err))
					mutex.Unlock()
					continue
				}
				escapes := opt.FrameEscapes(module)
				census := opt.AllocationCensus(module)
				name := filepath.Base(program)
				mutex.Lock()
				for _, escape := range escapes {
					note(audit.frameEscapes, normalizeCorpusKey(escape.Key()), name)
				}
				for _, allocation := range census {
					note(audit.allocations, normalizeCorpusKey(allocation.Key()), name)
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

	sort.Strings(audit.failures)
	return audit
}

// normalizeCorpusKey strips this checkout's directory from the stdlib paths a
// finding carries, so a baseline is the same file on every machine.
func normalizeCorpusKey(key string) string {
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

// readBaseline reads an accepted-baseline file: one key per line, with blank
// lines and # comments ignored.
func readBaseline(t *testing.T, path string) map[string]bool {
	t.Helper()

	contents, err := os.ReadFile(path)
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

// writeBaseline rewrites an accepted-baseline file from this run: the header,
// then every key in sorted order.
func writeBaseline(t *testing.T, path, header string, found map[string]string) {
	t.Helper()

	var builder strings.Builder
	builder.WriteString(header)
	for _, key := range sortedKeys(found) {
		builder.WriteString(key)
		builder.WriteString("\n")
	}
	require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o644))
}
