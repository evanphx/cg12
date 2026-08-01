package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// mspan.reportZombies is what the runtime prints when the sweeper finds an
// object that is marked in this cycle but was free at the end of the last one.
// It used to be blind on every span the Green Tea collector gives inline mark
// bits -- elemsize 16..512, which is most of the heap -- because mspan.sweep
// calls moveInlineMarks first, merging those bits into gcmarkBits and clearing
// them, and reportZombies then read them back through markBitsForBase, which
// returns the inline bits for such a span. It printed every object as unmarked,
// never printed the zombie line and never hexdumped the object, while still
// throwing "found pointer to free object".
//
// This test drives a program that puts a real zombie on a 32-byte span and
// requires the report to name it. See testdata/zombie_report_probe.go for why
// each ingredient of that program is needed.
const zombieReportProbe = "testdata/zombie_report_probe.go"

func TestReportZombiesNamesAZombieOnAnInlineMarkBitSpan(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		t.Skip("cg12 Go images require Linux ARM64")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("cc is required to assemble the Go runtime's Plan 9 sidecar")
	}

	compiler := sharedGOCBinary(t)
	program := filepath.Join(t.TempDir(), "zombie-report")
	build := exec.Command(compiler, "-o", program, zombieReportProbe)
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, "compile the zombie probe:\n%s", buildOutput)

	output, status := runImage(t, program)

	// The probe is supposed to die in the sweeper. Reaching its final print
	// means no zombie was ever created and the rest of the test proves nothing.
	require.NotContains(t, output, "no zombie was reported", "the probe did not create a zombie")
	require.Equal(t, 2, status, "program output:\n%s", output)
	require.Contains(t, output, "fatal error: found pointer to free object")

	// The span the fault lands on is the one with inline mark bits.
	require.Contains(t, output, "runtime: marked free object in span ")
	require.Contains(t, output, ", elemsize=32 ")

	// The defect's exact signature was a report in which nothing was marked.
	markedObjects := strings.Count(output, " marked  ")
	require.Greater(t, markedObjects, 0, "every object printed as unmarked:\n%s", output)

	// One object, free and marked, named as the zombie.
	zombieLines := strings.Count(output, " free  marked   zombie\n")
	require.Equal(t, 1, zombieLines, "the zombie was not named:\n%s", output)

	// And hexdumped, with the payload the probe wrote into object 40, which is
	// the object it hid in a uintptr. A report that named a neighbour instead
	// would still print a zombie line but would carry a different word here.
	require.Contains(t, output, "7a6f6d62 69650028", "the zombie was not dumped:\n%s", output)
}
