package goc_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A frame's pointer map says which of the frame's words the collector may treat
// as addresses. A word it claims wrongly is not a missed root, which merely
// collects something early: it is a scalar handed to the runtime as an address.
// What that costs depends on the value and on which of the two walkers that
// read the map gets there. runtime.adjustpointers, which the stack copier runs,
// throws on any non-zero value below the lowest legal pointer; the mark phase's
// runtime.findObject returns silently for an address that was never part of the
// heap. So a frame map can be wrong for a long time before a program dies of
// it, which is what the last program in the table records.
//
// log/slog is where that goes from a theoretical hazard to a crash in ordinary
// code. slog.Value packs int64, uint64, bool, time.Duration and float64 into a
// uint64 num field so those kinds never become heap-boxed interfaces
// (stdlib/src/log/slog/value.go), which makes slog.Value a struct mixing one
// scalar word with an any, and slog.Attr a struct whose only pointer words are
// the key's data pointer and that any. An attribute carrying 200 that is live
// in a frame when a collection happens dies with
//
//	runtime: bad pointer in frame main_main at 0x...: 0xc8
//	fatal error: invalid pointer found on stack
//
// which is 200 being walked as an address.
//
// These are the reductions from the log/slog allocation measurement, which
// found the defect. They are corpus programs so the compiler's answer is
// checked against the language's on every run, and the expectations are
// literals so the check holds where no host toolchain is installed;
// TestSlogAttrFrameExpectationsMatchTheHostToolchain proves the literals are
// `go run`'s own output rather than something written down by hand.
var slogAttrFramePrograms = []struct {
	source string
	want   string
}{
	// The reduction: slog.Int("k", 200) held in a frame across runtime.GC().
	{"slog_attr_frame_gcmask.go", "200\n"},
	// The same shape reached by the stack copier instead of the collector.
	{"slog_attr_frame_gcmask_stackcopy.go", "200\n"},
	// Every kind that packs into num, live in one frame at once.
	{"slog_attr_frame_gcmask_kinds.go", "200 true 3000000000 3\n"},
	// The same shape with no log/slog anywhere in the program: a zero-length
	// array field ahead of a scalar, held across a stack copy.
	{"slog_attr_frame_gcmask_shape.go", "200\n"},
	// That shape held across a collection instead. It passes on main -- the
	// mark phase walks past the claimed word rather than throwing on it -- so
	// it is here to fail if a fix for the programs above breaks the case that
	// already worked.
	{"slog_attr_frame_gcmask_control.go", "200\n"},
}

// TestSlogAttrInFrameIsNotScannedAsAPointer runs each reduction and compares
// what goc's program printed against what the language says it prints,
// unoptimized and optimized. Both configurations are run because the frame's
// pointer words are contributed from more than one place and an optimization
// pass can add or drop a contribution.
func TestSlogAttrInFrameIsNotScannedAsAPointer(t *testing.T) {
	for _, program := range slogAttrFramePrograms {
		for _, optimized := range []bool{false, true} {
			name := program.source
			if optimized {
				name += " -O"
			}
			t.Run(name, func(t *testing.T) {
				got := runCorpusProgramOutput(t, filepath.Join("testdata", program.source), optimized)
				require.Equal(t, program.want, got,
					"goc's answer differs from Go's: a word of a slog.Attr in the frame is\n"+
						"being scanned as a pointer, so the collector or the stack copier walks\n"+
						"the integer the attribute carries as an address")
			})
		}
	}
}

// TestSlogAttrInFrameSurvivesTheStackCopyChecker runs the same programs under
// GODEBUG=cg12checkstackcopy=1, which validates every word the frame's pointer
// map claims as the stack is copied and throws on one that is not an address in
// either stack. The plain run above catches a claimed scalar only when the
// value in it happens to be a plausible pointer; this one catches it whatever
// the value, and it is the diagnostic the bug reproduces through.
func TestSlogAttrInFrameSurvivesTheStackCopyChecker(t *testing.T) {
	for _, program := range slogAttrFramePrograms {
		t.Run(program.source, func(t *testing.T) {
			got := runCorpusProgramOutputWithEnv(t, filepath.Join("testdata", program.source), false,
				"GODEBUG=cg12checkstackcopy=1")
			require.Equal(t, program.want, got)
		})
	}
}

// TestSlogAttrFrameExpectationsMatchTheHostToolchain checks the expectations
// above against `go run`, so a wrong entry cannot make the corpus test enforce
// the wrong answer forever.
func TestSlogAttrFrameExpectationsMatchTheHostToolchain(t *testing.T) {
	toolchain, err := exec.LookPath("go")
	if err != nil {
		t.Skip("host Go toolchain unavailable")
	}
	for _, program := range slogAttrFramePrograms {
		t.Run(program.source, func(t *testing.T) {
			command := exec.Command(toolchain, "run", filepath.Join("testdata", program.source))
			output, err := command.CombinedOutput()
			require.NoError(t, err, "go run: %s", output)
			require.Equal(t, program.want, string(output))
		})
	}
}
