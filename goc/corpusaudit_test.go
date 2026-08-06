package goc_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
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
	// loopAliases is opt.LoopAliases's findings: every allocation a loop body
	// leaves in a frame slot whose address outlives the iteration that made it.
	// See TestLoopAliasAudit.
	loopAliases map[string]string
	// allocations is opt.AllocationCensus's records. See TestAllocationCensus.
	allocations map[string]string
	// shadow is opt.ShadowPlacement's disagreements: every allocation goc's AST
	// walk and the summary-fed IR analysis place differently. See
	// TestEscapeShadowPlacement.
	shadow map[string]string
	// placements is every front-end placement by site identity, which is the
	// denominator the distinct disagreement count belongs over.
	placements map[string]string
	// shadowCounts is the totals those disagreements came out of, summed over
	// the corpus. A program compiled twice contributes twice, which is what makes
	// it a total rather than a count of distinct sites.
	shadowCounts opt.ShadowCounts
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
// corpus-wide run does not multiply goc's peak by the core count.
//
// The bound used to be a flat 8. That was the right number on the machine it was
// written on and badly wrong on a 64-core, 243 GiB worker, where it made this
// the single longest test in the tree: 183 s of the corpus suite's 1121 s, with
// 56 cores idle. It is now bounded by memory the same way the capability matrix
// bounds its compile fan-out -- MemAvailable divided by a measured per-compile
// peak -- so the same code is right on both machines and neither caller has to
// pass anything.
//
// auditCompilePeakBytes is the corpus's worst observed compile, not its average:
// 4.23 GiB at GOMAXPROCS=1 for stdlib_http_tls_client_server.go. Sizing on the
// worst case is the point -- the pool has no way to know which program a worker
// will draw next, so a bound built on the average is a bound that is wrong
// exactly when several expensive programs land together.
//
// The tests that read this audit are all listed in sequential_tests.txt, so this
// pool has the process to itself; it does not have to leave room for the
// parallel half of the suite. GOC_AUDIT_WORKERS overrides it.
const auditCompilePeakBytes = 4.23 * (1 << 30)

func auditCompileWorkers() int {
	if setting := os.Getenv("GOC_AUDIT_WORKERS"); setting != "" {
		if workers, err := strconv.Atoi(setting); err == nil && workers > 0 {
			return workers
		}
	}
	workers := runtime.GOMAXPROCS(0)
	if available := availableMemoryBytes(); available > 0 {
		if byMemory := int(float64(available) / auditCompilePeakBytes); byMemory < workers {
			workers = byMemory
		}
	}
	return max(workers, 1)
}

// availableMemoryBytes reports MemAvailable, or 0 when it cannot be read.
// MemAvailable rather than MemFree deliberately: page cache is reclaimable, and
// treating it as unavailable would serialize the audits on any machine that has
// been doing I/O -- which, after a corpus compile, is every machine.
func availableMemoryBytes() uint64 {
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(meminfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemAvailable:" {
			continue
		}
		kilobytes, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kilobytes * 1024
	}
	return 0
}

func compileCorpusForAudits(programs []string) *corpusAudit {
	workers := auditCompileWorkers()

	audit := &corpusAudit{
		programs:     len(programs),
		frameEscapes: make(map[string]string),
		loopAliases:  make(map[string]string),
		allocations:  make(map[string]string),
		shadow:       make(map[string]string),
		placements:   make(map[string]string),
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
				// The other half of the escape decision's correctness, and the
				// half FrameEscapes is structurally blind to: an allocation left
				// in a frame slot inside a loop body whose address outlives the
				// iteration. Nothing is published in that case, so there is no
				// store for a publication audit to look at. See opt.LoopAliases.
				aliases := opt.LoopAliases(module)
				// The census records the front end's own frame placements as well
				// as the IR pass's, so that an object moving between a front-end
				// frame slot and the heap is one line changing placement rather
				// than a line vanishing and an unrelated one appearing. See
				// opt.AllocationCensusOptions.
				census := opt.AllocationCensusWith(module, opt.AllocationCensusOptions{
					IncludeFrontEndFrameSlots: true,
				})
				// Shadow mode reads the finished module and changes nothing in
				// it. The compile above is the shipping configuration --
				// summaries on -- so what is being compared is the code the
				// compiler actually emitted against what the same summary-fed
				// analysis would have chosen for the placements the front end
				// kept for itself.
				disagreements, counts := opt.ShadowPlacement(module, opt.ComputeEscapeFacts(module))
				name := filepath.Base(program)
				mutex.Lock()
				for _, escape := range escapes {
					note(audit.frameEscapes, normalizeCorpusKey(escape.Key()), name)
				}
				for _, alias := range aliases {
					note(audit.loopAliases, normalizeCorpusKey(alias.Key()), name)
				}
				for _, allocation := range census {
					note(audit.allocations, normalizeCorpusKey(allocation.Key()), name)
				}
				for _, disagreement := range disagreements {
					note(audit.shadow, normalizeCorpusKey(disagreement.Key()), name)
				}
				for _, site := range opt.FrontEndPlacementSites(module) {
					note(audit.placements, normalizeCorpusKey(site), name)
				}
				audit.shadowCounts.Placements += counts.Placements
				audit.shadowCounts.FrontFrame += counts.FrontFrame
				audit.shadowCounts.FrontHeap += counts.FrontHeap
				audit.shadowCounts.Agree += counts.Agree
				audit.shadowCounts.FrameToIRHeap += counts.FrameToIRHeap
				audit.shadowCounts.HeapToIRFrame += counts.HeapToIRFrame
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
