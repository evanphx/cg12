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
	"github.com/evanphx/cg12/ir"
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
	// verifyFailures is every ir.Verify diagnostic the corpus produces, keyed by
	// the diagnostic itself -- which names one function, so the corpus's shared
	// stdlib contributes each failing function once rather than once per program.
	// See TestIRVerifyAudit.
	verifyFailures map[string]string
	// functions is how many verifications ran, summed over the corpus. It is not
	// the denominator of verifyFailures, which is deduplicated; it is the size of
	// the sweep.
	functions int
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
		programs:       len(programs),
		frameEscapes:   make(map[string]string),
		loopAliases:    make(map[string]string),
		allocations:    make(map[string]string),
		shadow:         make(map[string]string),
		placements:     make(map[string]string),
		verifyFailures: make(map[string]string),
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
				// ir.Verify is an instrument the tree already had and did not
				// run on its own output: nothing between the front end and the
				// backend called it, so the only callers were the binary decoder
				// and the lifter, neither of which sees a goc compile. 4-6% of
				// the functions goc emitted failed it and nothing said so. It
				// costs one linear walk per function against a compile that
				// dominates everything here, so the corpus pass is where it
				// belongs. See TestIRVerifyAudit.
				var rejected []string
				for _, function := range module.Funcs {
					if err := ir.Verify(function); err != nil {
						rejected = append(rejected, err.Error())
					}
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
				audit.functions += len(module.Funcs)
				for _, failure := range rejected {
					note(audit.verifyFailures, normalizeCorpusKey(failure), name)
				}
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
