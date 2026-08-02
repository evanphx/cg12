package goc_test

import (
	"flag"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
	found := auditCorpus(t).frameEscapes

	if *updateFrameEscapeBaseline {
		writeBaseline(t, frameEscapeBaselinePath, frameEscapeBaselineHeader, found)
		t.Skip("baseline rewritten; rerun without -update-frame-escape-baseline to check it")
	}

	accepted := readBaseline(t, frameEscapeBaselinePath)

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
