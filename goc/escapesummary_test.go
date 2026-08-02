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
	"time"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateEscapeShadowBaseline = flag.Bool("update-escape-shadow-baseline", false,
	"rewrite testdata/escape_shadow_baseline.txt from this run")

var measurePromotionRate = flag.Bool("escape-promotion-rate", false,
	"compile the whole corpus with and without the summary knob and report the promotion rate")

var escapeSummarySymbol = flag.String("escape-summary-symbol", "",
	"print the summary of every symbol whose name contains this substring")

var escapeSummaryProgram = flag.String("escape-summary-program", "testdata/stdlib_crypto_ecdsa.go",
	"the program TestEscapeSummaryCost and TestEscapeSummaryFacts compile")

const escapeShadowBaselinePath = "testdata/escape_shadow_baseline.txt"

// TestEscapeShadowPlacement records every allocation goc's AST walk and the
// summary-fed IR analysis place differently, over the whole corpus.
//
// This is the artifact that decides whether placement can move onto the IR. The
// compiler is not changed by it: the corpus is compiled exactly as it ships,
// and opt.ShadowPlacement then asks the IR analysis what it would have done
// with each allocation the front end placed itself.
//
// The two directions of disagreement are different kinds of news and are
// counted separately:
//
//   - front end frame -> IR heap is the conservative direction. Each one is an
//     allocation that appears if that decision site is routed through the
//     neutral op, so the total is the price of the migration. Its Reason field
//     names the use that escaped it, which is what says whether the IR analysis
//     is missing a fact or the allocation genuinely has to be on the heap.
//   - front end heap -> IR frame is the permissive direction, and it is the
//     dangerous one. Each is either a latent pessimisation in the AST walk or a
//     hole in the IR analysis, and nothing may switch until every class has been
//     argued. ccwork/escape-analysis's 2724ac7 is what a wrong answer here costs:
//     a dead frame address inside a live heap object, found minutes later by an
//     unrelated goroutine's collector.
func TestEscapeShadowPlacement(t *testing.T) {
	audit := auditCorpus(t)

	counts := audit.shadowCounts
	t.Logf("front-end placements evaluated: %d (frame %d, heap %d)",
		counts.Placements, counts.FrontFrame, counts.FrontHeap)
	t.Logf("agree %d; front frame -> IR heap %d; front heap -> IR frame %d",
		counts.Agree, counts.FrameToIRHeap, counts.HeapToIRFrame)
	frame, heap := 0, 0
	for key := range audit.placements {
		if strings.HasSuffix(key, "\t"+string(ir.AllocInFrame)) {
			frame++
		} else {
			heap++
		}
	}
	t.Logf("distinct front-end placement sites: %d (frame %d, heap %d)", len(audit.placements), frame, heap)
	t.Logf("distinct disagreement sites: %d", len(audit.shadow))
	for _, line := range classifyShadowDisagreements(audit.shadow) {
		t.Log(line)
	}

	if *updateEscapeShadowBaseline {
		writeBaseline(t, escapeShadowBaselinePath, escapeShadowBaselineHeader, audit.shadow)
		t.Skip("baseline rewritten; rerun without -update-escape-shadow-baseline to check it")
	}

	accepted := readBaseline(t, escapeShadowBaselinePath)
	var appeared, vanished []string
	for _, key := range sortedKeys(audit.shadow) {
		if !accepted[key] {
			appeared = append(appeared, key+"\n      seen compiling "+audit.shadow[key])
		}
	}
	for _, key := range sortedSet(accepted) {
		if _, found := audit.shadow[key]; !found {
			vanished = append(vanished, key)
		}
	}
	assert.Empty(t, appeared,
		"the IR analysis disagrees with goc's AST walk at a site %s does not list. Read each one\n"+
			"before accepting it: a new `heap -> frame` line is a permissive change and needs an\n"+
			"argument for why the object cannot outlive the call.\n  %s",
		escapeShadowBaselinePath, strings.Join(appeared, "\n  "))
	assert.Empty(t, vanished,
		"%s lists a disagreement the run no longer produces. That is progress if the two analyses\n"+
			"now agree and a lost measurement if the site is simply gone.\n  %s",
		escapeShadowBaselinePath, strings.Join(vanished, "\n  "))
}

// classifyShadowDisagreements groups the disagreements by direction and by the
// reason the IR analysis gave, which is the form a reviewer reads. A count of
// lines says nothing; a count per class says which fact is missing.
func classifyShadowDisagreements(shadow map[string]string) []string {
	classes := make(map[string]int)
	for key := range shadow {
		fields := strings.Split(key, "\t")
		if len(fields) < 6 {
			continue
		}
		loop := ""
		if fields[4] != "" {
			loop = ", " + fields[4]
		}
		if fields[5] == "" {
			// The permissive direction has no single use to name, so it is
			// classified by the front-end decision site instead -- and by whether
			// the allocation is in a loop, which is what says whether promoting it
			// would be safe.
			classes[fields[3]+"\tat "+fields[2]+loop]++
			continue
		}
		classes[fields[3]+"\t"+fields[5]+loop]++
	}
	names := make([]string, 0, len(classes))
	for name := range classes {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if classes[names[i]] != classes[names[j]] {
			return classes[names[i]] > classes[names[j]]
		}
		return names[i] < names[j]
	})
	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, fmt.Sprintf("  %6d  %s", classes[name], strings.ReplaceAll(name, "\t", "  ")))
	}
	return lines
}

const escapeShadowBaselineHeader = `# Every allocation goc's AST escape walk and the summary-fed IR escape analysis
# place differently, over the whole corpus, one per line:
#
#     source position <TAB> function <TAB> decision site <TAB> front end -> IR <TAB> in a loop? <TAB> reason
#
# Nothing here changes what the compiler emits. The corpus is compiled exactly as
# it ships and opt.ShadowPlacement then asks the IR analysis what it would have
# decided about each allocation the front end placed itself. The reason field is
# empty when the IR analysis kept the object in a frame -- there is no single use
# to name for that -- and otherwise names the use that escaped it.
#
# The "in a loop?" column is the one that matters on a heap -> frame line.
# Neither analysis knows what an iteration is: both answer "does a pointer
# outlive the frame", and an allocation in a loop body can be entirely
# frame-local by that test and still need one object per iteration. Promoting it
# puts every iteration on one slot. See ESCAPE_IR_PLAN.md section 5.
#
# The two directions are not the same kind of news:
#
#   frame -> heap  the IR analysis is more conservative. Each line is an
#                  allocation that would appear if that site were routed through
#                  the neutral op, so the count is the price of the migration.
#
#   heap -> frame  the IR analysis is more permissive. Each line is either a
#                  pessimisation in the AST walk or a hole in the new analysis,
#                  and until it is known which, it is a potential miscompile of
#                  exactly the shape 2724ac7 and 9f76498 were.
#
# Regenerate with
#
#     go test ./goc -run TestEscapeShadowPlacement -update-escape-shadow-baseline
#
# and read the diff.
`

// TestEscapeSummaryFacts reports the shape of the fact table on one
// representative executable: how many parameters get each answer, how much of
// the call graph is recursive, and what the fixed point buys over the AST
// walk's cycle break.
func TestEscapeSummaryFacts(t *testing.T) {
	module := compileEscapeSummaryProgram(t)

	facts := opt.ComputeEscapeFacts(module)
	broken := opt.ComputeEscapeFactsBreakingCycles(module)
	stats := facts.Stats

	t.Logf("program %s", *escapeSummaryProgram)
	t.Logf("functions %d (with a body %d), parameters %d", stats.Funcs, stats.Bodied, stats.Params)
	t.Logf("verdicts: noescape %d, leaks-to-result %d, escapes %d",
		stats.NoEscape, stats.LeaksToResult, stats.Escapes)
	t.Logf("call graph: %d components, %d non-trivial (%d functions, largest %d), %d self-recursive",
		stats.Components, stats.NonTrivialSCCs, stats.FuncsInNonTrivialSCCs, stats.LargestSCC,
		stats.SelfRecursiveFuncs)
	t.Logf("fixed point: %d rounds, %d re-analyses beyond the first, %d bail-outs",
		stats.Rounds, stats.Requeries, stats.Bailouts)
	t.Logf("//go:noescape: %d declared symbols, %d parameters answered from a bodiless ir.Func",
		stats.NoEscapeSymbols, stats.Directives)

	if *escapeSummarySymbol != "" {
		for _, symbol := range facts.Symbols() {
			if !strings.Contains(symbol, *escapeSummarySymbol) {
				continue
			}
			parts := make([]string, 0, 4)
			for index, fact := range facts.Params(symbol) {
				name := fact.Escape.String()
				if fact.Escape == opt.ParamLeaksToResult {
					name = fmt.Sprintf("%s(%d)", name, fact.Result)
				}
				parts = append(parts, fmt.Sprintf("%d:%s", index, name))
			}
			t.Logf("summary $%s: %s", symbol, strings.Join(parts, " "))
		}
	}

	disagreements, counts := opt.ShadowPlacement(module, facts)
	t.Logf("shadow: %d front-end placements (frame %d, heap %d); agree %d; front frame -> IR heap %d; front heap -> IR frame %d",
		counts.Placements, counts.FrontFrame, counts.FrontHeap, counts.Agree,
		counts.FrameToIRHeap, counts.HeapToIRFrame)
	shadow := make(map[string]string, len(disagreements))
	for _, disagreement := range disagreements {
		shadow[disagreement.Key()] = ""
	}
	for _, line := range classifyShadowDisagreements(shadow) {
		t.Log(line)
	}

	improved, regressed := compareFactTables(module, facts, broken)
	t.Logf("fixed point vs the AST walk's cycle break: %d parameters improved, %d worse", improved, regressed)

	require.Zero(t, stats.Bailouts, "a component that hits the round cap is a transfer function that is not monotone")
	assert.Zero(t, regressed, "the fixed point may only be more precise than breaking cycles, never less")
}

// compareFactTables counts the parameters the fixed point answers better, and
// the ones it answers worse -- which must be none: an optimistic fixed point
// over a monotone transfer function is at least as precise as breaking every
// cycle at "escapes".
func compareFactTables(module *ir.Module, fixedPoint, broken *opt.EscapeFacts) (improved, regressed int) {
	for _, function := range module.Funcs {
		if function.Start == nil {
			continue
		}
		for index := range function.Params {
			solved := fixedPoint.Param(function.Name, index).Escape
			cut := broken.Param(function.Name, index).Escape
			switch {
			case solved > cut:
				improved++
			case solved < cut:
				regressed++
			}
		}
	}
	return improved, regressed
}

// TestEscapeSummaryCost prices the fact table against the AST walk it is meant
// to replace the interprocedural half of.
//
// The reference from ccwork/escape-ir-design at ddd03eb, on
// stdlib_crypto_ecdsa.go: the AST walk is 0.456 s of a 6.47 s compile (7.1%),
// of which about 0.28 s is astParents rebuilding a parent map for each of 12 612
// summary queries over 1 713 256 AST nodes -- 3.91x the whole program's AST -- to
// answer 1 485 distinct questions 8.51 times each.
//
// The IR table answers each question once, by construction: it is a map keyed by
// symbol, filled by one bottom-up pass over the call graph. There is nothing to
// memoize because nothing is asked twice.
// It also prices what enabling the table costs a whole compile, which is the
// number that matters now that it is on by default: the table itself, plus the
// larger analysis the pass then runs, plus the loop forests promotionsBlockedByALoop
// builds for functions that have something to promote.
func TestEscapeSummaryCost(t *testing.T) {
	source, err := os.ReadFile(*escapeSummaryProgram)
	require.NoError(t, err)

	const rounds = 3
	compileWith := func(summaries bool) (time.Duration, *ir.Module) {
		previous := opt.EscapeSummaries
		opt.EscapeSummaries = summaries
		defer func() { opt.EscapeSummaries = previous }()

		var total time.Duration
		var module *ir.Module
		for round := 0; round < rounds; round++ {
			start := time.Now()
			compiled, err := goc.CompileExecutable(*escapeSummaryProgram, source)
			require.NoError(t, err)
			total += time.Since(start)
			module = compiled
		}
		return total / rounds, module
	}

	off, _ := compileWith(false)
	on, module := compileWith(true)

	var table time.Duration
	for round := 0; round < rounds; round++ {
		start := time.Now()
		opt.ComputeEscapeFacts(module)
		table += time.Since(start)
	}
	table /= rounds

	functions := 0
	for _, function := range module.Funcs {
		if function.Start != nil {
			functions++
		}
	}
	stats := opt.HeapAllocLoweringStats(module)
	t.Logf("program %s: %d functions with a body", *escapeSummaryProgram, functions)
	t.Logf("compile, summaries off (mean of %d): %.3f s", rounds, off.Seconds())
	t.Logf("compile, summaries on  (mean of %d): %.3f s = %+.2f%%",
		rounds, on.Seconds(), 100*(on.Seconds()-off.Seconds())/off.Seconds())
	t.Logf("ComputeEscapeFacts (mean of %d): %.3f s = %.2f%% of the summaries-off compile",
		rounds, table.Seconds(), 100*table.Seconds()/off.Seconds())
	t.Logf("this program: promoted %d, lowered %d (%d of them blocked by the loop rule)",
		stats.Promoted, stats.Lowered, stats.LoopBlocked)
}

// TestEscapeSummaryPromotionRate compiles the whole corpus with the summary
// knob on and reports what LowerHeapAllocations then promotes, against the same
// number with the knob off.
//
// It is the acceptance number for the fact table: 3.1% (14 433 of 467 752) is
// what an intraprocedural pass with no callee summaries manages, because
// opt/escape.go's default arm escapes every candidate that reaches a call it
// does not recognise, and most allocations reach a call. If the rate is still
// near 3% with the table wired in, the table is not doing its job.
//
// The corpus is compiled twice, which takes tens of minutes, so it is skipped in
// short mode.
func TestEscapeSummaryPromotionRate(t *testing.T) {
	// Compiling the corpus twice is tens of minutes, which would dominate every
	// ordinary `go test ./goc/...` run, and it also has to move a package-level
	// knob that nothing else may see move. Both reasons say it is asked for
	// explicitly rather than run by default.
	if !*measurePromotionRate {
		t.Skip("pass -escape-promotion-rate to compile the corpus twice and measure")
	}
	programs, err := filepath.Glob("testdata/*.go")
	require.NoError(t, err)
	require.NotEmpty(t, programs)
	sort.Strings(programs)

	off := compileCorpusForLoweringStats(t, programs, false)
	on := compileCorpusForLoweringStats(t, programs, true)

	t.Logf("knob off: promoted %d, lowered %d, rate %.2f%%, blocked by the loop rule %d",
		off.Promoted, off.Lowered, 100*off.Rate(), off.LoopBlocked)
	t.Logf("knob on:  promoted %d, lowered %d, rate %.2f%%, blocked by the loop rule %d",
		on.Promoted, on.Lowered, 100*on.Rate(), on.LoopBlocked)
	require.Equal(t, off.Promoted+off.Lowered, on.Promoted+on.Lowered,
		"the two runs must see the same candidates, or they are not comparable")
	assert.Greater(t, on.Promoted, off.Promoted, "the fact table must promote more than no table at all")
}

// compileCorpusForLoweringStats compiles every program with the summary knob in
// one position and sums what the lowering pass decided.
func compileCorpusForLoweringStats(t *testing.T, programs []string, summaries bool) opt.HeapAllocLowering {
	t.Helper()

	previous := opt.EscapeSummaries
	opt.EscapeSummaries = summaries
	defer func() { opt.EscapeSummaries = previous }()

	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	var (
		mutex sync.Mutex
		total opt.HeapAllocLowering
		bad   []string
	)
	work := make(chan string)
	var working sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		working.Add(1)
		go func() {
			defer working.Done()
			for program := range work {
				source, err := os.ReadFile(program)
				if err == nil {
					var module *ir.Module
					module, err = goc.CompileExecutable(program, source)
					if err == nil {
						stats := opt.HeapAllocLoweringStats(module)
						mutex.Lock()
						total.Promoted += stats.Promoted
						total.Lowered += stats.Lowered
						mutex.Unlock()
						continue
					}
				}
				mutex.Lock()
				bad = append(bad, fmt.Sprintf("%s: %v", program, err))
				mutex.Unlock()
			}
		}()
	}
	for _, program := range programs {
		work <- program
	}
	close(work)
	working.Wait()

	require.Empty(t, bad, "every corpus program must compile for the rate to mean anything")
	return total
}

// compileEscapeSummaryProgram compiles the one representative program the
// summary tests report on.
func compileEscapeSummaryProgram(t *testing.T) *ir.Module {
	t.Helper()
	source, err := os.ReadFile(*escapeSummaryProgram)
	require.NoError(t, err)
	module, err := goc.CompileExecutable(*escapeSummaryProgram, source)
	require.NoError(t, err)
	return module
}
