package goc_test

import (
	"strings"
	"testing"

	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two summary-safety reductions, compiled from Go rather than built as IR
// by hand.
//
// opt/escapefacts_test.go and opt/escape_test.go state the same two defects
// directly on the IR, which is where the fixes are and which is what fails when
// they are reverted. These two say the same thing through the front end, so the
// property is tied to code goc actually emits -- the calling convention that
// passes a struct by frame address, and the write barrier it emits for a pointer
// field of a local -- rather than to an IR shape a reader has to be told is
// realistic.
//
// Both assert the same end property as well as the mechanism: the IR analysis
// must not be more permissive than the front end anywhere in the reduction. That
// is the direction CCWORK_REPORT.md section 2 is about, and the direction
// ccwork/escape-analysis's 2724ac7 got wrong.

// A pointer passed to a callee inside a struct the caller owns is retained by
// that callee.
//
// This is CCWORK_REPORT.md section 2.2's reduction, source and all. request has
// four pointer fields deliberately: with a scalar tail goc copies the tail with
// goc_memcpy, escapeGraph.copyMemory publishes the whole source region, and the
// pointer escapes by accident rather than by rule -- which is the accident that
// stood between the first draft of this reduction and a miscompile.
//
// publish's own summary is correct and nearly useless: it does not retain the
// frame address it was handed, only the pointer it loads out of it. hold's
// summary is the one that has to be right, because hold's caller is what decides
// where the object goes.
//
// It is compiled to an executable, not just a package. Both reductions are: the
// aggregate calling convention and the write barrier are what the defects are
// about, and a package compiled on its own gets neither.
func TestEscapeSummaryPointerPassedInsideAStructEscapes(t *testing.T) {
	module, err := goc.CompileExecutable("aggescape.go", []byte(`
package main

type node struct{ n int }

type request struct{ ptr, a, b, c *node }

var sink *node

func publish(r request) { sink = r.ptr }

func hold(x *node) { publish(request{ptr: x}) }

func stash() { hold(&node{n: 42}) }

func Test() int {
	stash()
	return sink.n
}

func main() { println(Test()) }
`))
	require.NoError(t, err)

	facts := opt.ComputeEscapeFacts(module)
	publish := summarisedSymbol(t, facts, "main.publish")
	hold := summarisedSymbol(t, facts, "main.hold")

	assert.Equal(t, opt.ParamNoEscape, facts.Param(publish, 0).Escape,
		"publish keeps the pointer it loads out of the struct, not the frame address it was handed")
	assert.NotEqual(t, opt.ParamNoEscape, facts.Param(hold, 0).Escape,
		"x is written into a struct publish loads through, so publish retains x past the call:\n"+
			"a noescape here puts &node{n: 42} in stash's frame and leaves sink holding a dead frame address")
	assertNoPermissiveDisagreement(t, module, facts)
}

// A slice backing array written into its header through the write barrier and
// then captured by a closure that outlives the loop stays on the heap.
//
// This is CCWORK_REPORT.md section 3.5's second hole, reduced from the corpus row
// at runtime_loopvar_value_shapes.go:33:17 to ten lines. The allocation is in the
// for-init, so it runs once and the loop rule has nothing to say about it; what
// keeps it off the frame is that the closures appended to a package-level slice
// read it after Test returns. gc agrees: `[]int{...} escapes to heap`.
//
// The header is an alloc8 24 the front end addresses directly, which is what
// makes this the barrier case rather than the ordinary local-variable case --
// goc double-indirects a named local, and a load of the pointer-to-storage slot
// already defeats locOf, so the object escapes by a blunter route there.
//
// It is compiled to an executable, not just a package: the write barrier is only
// emitted against the real runtime, and with calloc in its place the reduction
// compiles, passes, and tests nothing.
func TestEscapeSummarySliceBackingBarrieredIntoItsHeaderIsStillTracked(t *testing.T) {
	module, err := goc.CompileExecutable("barrierescape.go", []byte(`
package main

var captured []func() int

func Test() int {
	for numbers := []int{0}; len(numbers) < 4; numbers = append(numbers, len(numbers)) {
		captured = append(captured, func() int { return len(numbers) })
	}
	return len(captured)
}

func main() { println(Test()) }
`))
	require.NoError(t, err)

	assert.GreaterOrEqual(t, countCallsSymbol(moduleFunc(t, module, "main.Test"), "goc_storep"), 1,
		"the reduction is about the barrier path; with no barrier it proves nothing")
	assertNoPermissiveDisagreement(t, module, opt.ComputeEscapeFacts(module))
}

// assertNoPermissiveDisagreement fails if the IR analysis would put any
// allocation in a frame that goc's AST walk put on the heap.
//
// The conservative direction is left alone deliberately: it costs an allocation
// and nothing else, and the AST walk is documented to be pessimistic in ways
// section 3.3 enumerates. Only the permissive direction can be a miscompile.
func assertNoPermissiveDisagreement(t *testing.T, module *ir.Module, facts *opt.EscapeFacts) {
	t.Helper()

	disagreements, counts := opt.ShadowPlacement(module, facts)
	require.NotZero(t, counts.Placements, "the front end placed nothing, so there is nothing to check")

	var permissive []string
	for _, disagreement := range disagreements {
		if disagreement.IR == ir.AllocInFrame {
			permissive = append(permissive, disagreement.Key())
		}
	}
	assert.Empty(t, permissive,
		"the IR analysis would promote an allocation goc's AST walk put on the heap.\n"+
			"Each line is an object that would move into a frame if this analysis decided it;\n"+
			"in this reduction that frame outlives nothing and a live object holds its address.\n  %s",
		strings.Join(permissive, "\n  "))
}

// summarisedSymbol finds the one module symbol whose name ends in want. The
// front end decorates package-level names, so a test that hard-codes the
// decoration breaks on an unrelated naming change.
func summarisedSymbol(t *testing.T, facts *opt.EscapeFacts, want string) string {
	t.Helper()

	var found []string
	for _, symbol := range facts.Symbols() {
		if symbol == want || strings.HasSuffix(symbol, "."+want) {
			found = append(found, symbol)
		}
	}
	require.Len(t, found, 1, "expected exactly one symbol named %s, got %v", want, found)
	require.NotEmpty(t, facts.Params(found[0]), "%s must be summarised for the test to mean anything", found[0])
	return found[0]
}

// moduleFunc finds the one module function whose name ends in want.
func moduleFunc(t *testing.T, module *ir.Module, want string) *ir.Func {
	t.Helper()

	var found []*ir.Func
	for _, function := range module.Funcs {
		if function.Name == want || strings.HasSuffix(function.Name, "."+want) {
			found = append(found, function)
		}
	}
	require.Len(t, found, 1, "expected exactly one function named %s", want)
	return found[0]
}
