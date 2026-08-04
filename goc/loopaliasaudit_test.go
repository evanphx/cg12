package goc_test

import (
	"flag"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// updateLoopAliasBaseline rewrites the accepted baseline from this run instead
// of comparing against it. Use it after a change that deliberately alters which
// loop-body allocations stay in a frame, and read the diff before committing it:
// every line is an object the compiler gave one frame slot where the source asks
// for one per iteration.
var updateLoopAliasBaseline = flag.Bool("update-loop-alias-baseline", false,
	"rewrite testdata/loop_alias_baseline.txt from this run")

const loopAliasBaselinePath = "testdata/loop_alias_baseline.txt"

// TestLoopAliasAudit compiles every corpus program and asks the finished IR the
// one question neither escape analysis asks: does an allocation a loop body
// leaves in a frame slot have an address that outlives the iteration that made
// it?
//
// The check is opt.LoopAliases; this test is what makes it run. It exists
// because the other whole-corpus audits are structurally blind to the defect,
// not merely silent about it. opt.FrameEscapes reports a frame address stored
// somewhere that outlives the *frame*; when two iterations share one slot
// nothing is published at all -- both pointers stay inside the frame and are
// simply the same pointer -- so there is no store for it to see. The allocation
// census records where each object went, and one frame slot per loop is exactly
// what it expects. Neither can be extended to cover this; the question is a
// different one.
//
// It was a live miscompile on this tree, in four allocation forms at both -O
// settings: goc printed `2 2` where Go prints `1 2`. The fix is the front end's
// per-iteration question (goc's findIterationCaptures) and the IR pass's loop
// rule (opt's promotionsBlockedByALoop), and until this test the only thing
// standing between that fix and a silent regression was three reduction programs
// in testdata, run and compared against what Go prints. Nothing audited the
// corpus for the shape at all.
//
// The baseline is a list of the loop-carried aliases this tree can make, not a
// certificate that they are harmless. Every line on it today is goc's indirect
// representation of a string variable -- the variable's slot holds a pointer to
// a sixteen-byte header value, and assigning to it inside a loop points the slot
// at a temporary the loop body allocates. Each was read and is benign for a
// reason the audit deliberately does not try to prove: the temporary is a fresh
// copy of a value rather than an object with identity, it has exactly one
// holder, and it is rewritten at the same site that re-points the holder. See
// the report in CCWORK_REPORT.md for the entry-by-entry triage.
//
// The test is a ratchet: it fails on an alias that is not already listed, and on
// a listed alias that has gone away, so the file cannot drift away from what the
// compiler does.
func TestLoopAliasAudit(t *testing.T) {
	found := auditCorpus(t).loopAliases

	if *updateLoopAliasBaseline {
		writeBaseline(t, loopAliasBaselinePath, loopAliasBaselineHeader, found)
		t.Skip("baseline rewritten; rerun without -update-loop-alias-baseline to check it")
	}

	accepted := readBaseline(t, loopAliasBaselinePath)

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
		"an allocation in a loop body is in a frame slot whose address outlives the iteration,\n"+
			"in a place %s does not list.\n"+
			"Each line is one frame slot standing in for what the source says is one object per\n"+
			"trip round the loop. Where two iterations' objects are live at once that is a wrong\n"+
			"answer printed by a program with nothing failing anywhere -- no publication for\n"+
			"TestFrameEscapeAudit to see and nothing the allocation census can distinguish from a\n"+
			"correct promotion. Move the allocation to the heap, or -- if the sharing is\n"+
			"unobservable -- explain why and rerun with -update-loop-alias-baseline.\n  %s",
		loopAliasBaselinePath, strings.Join(appeared, "\n  "))
	assert.Empty(t, vanished,
		"%s lists a loop-carried alias the compiler no longer makes.\n"+
			"That is usually good news; rerun with -update-loop-alias-baseline to record it.\n  %s",
		loopAliasBaselinePath, strings.Join(vanished, "\n  "))
}

const loopAliasBaselineHeader = `# Allocations this tree leaves in a frame slot inside a loop body while a
# pointer to that slot outlives the iteration that made it, one per line, as
# opt.LoopAliases reports them: where the object is allocated, the function, how
# the address survives the iteration, what held it, and where it crosses. IR
# temporary numbers are deliberately not part of a line, so an unrelated change
# that emits one more instruction does not rewrite the file.
#
# This is the defect class the other audits cannot see. FrameEscapes asks
# whether a frame address was published past its frame; here nothing is
# published, both pointers stay inside the frame and are simply the same
# pointer. The allocation census expects one frame slot per loop and cannot tell
# a correct promotion from this one. What is wrong is that the source asks for
# one object per trip round the loop and a frame slot is one object.
#
# This is a record of what the compiler does, not a list of things that are
# correct. TestLoopAliasAudit fails on any alias not listed here and on any
# listed alias that has gone away, so the file tracks the compiler rather than
# drifting from it. Regenerate with
#
#     go test ./goc -run TestLoopAliasAudit -update-loop-alias-baseline
#
# and read the diff: every added line is an object two iterations can share.
`
