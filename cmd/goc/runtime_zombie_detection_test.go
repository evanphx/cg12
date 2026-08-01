package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Zombie detection, RUNTIME_PLAN.md section 6's "zombie detection in a
// controlled negative subprocess".
//
// A zombie is an object the sweeper finds marked but free. sweepLocked.sweep
// tests gcmarkBits &^ allocBits after moving a Green Tea span's inline marks
// into gcmarkBits, and calls mspan.reportZombies, which throws. It is the
// runtime's last line of defence: the two faults RUNTIME_PLAN sections 5.11 and
// 5.12 fixed, and the channel-buffer defect this branch fixed, were all first
// seen through it.
//
// A check that only ever fires by accident is not known to work. This test makes
// one fire on purpose. The subprocess launders a pointer through a uintptr --
// deliberately breaking the unsafe.Pointer rules, which is exactly case 1 in
// reportZombies' own list of causes -- collects until the object is swept, and
// only then publishes the integer back as a pointer where the collector will
// follow it. The next cycle marks a free slot and the sweep after it throws.
//
// The control compiles and runs the same program with the laundering removed and
// requires it to exit 0, so a test that merely detected "the subprocess died"
// would fail.

const zombieProgramTemplate = `package main

import (
	"runtime"
	"unsafe"
)

// victim's size class is chosen to be one a program this small does not
// otherwise allocate from, so the freed slot is still free when it is marked.
type victim struct {
	filler [37]uintptr
	self   *victim
}

type holder struct {
	pointer unsafe.Pointer
}

var published = &holder{}
var laundered uintptr

//go:noinline
func makeVictim() uintptr {
	object := &victim{}
	object.filler[0] = 0x5ea1ed
	object.self = object
	return uintptr(unsafe.Pointer(object))
}

func main() {
	laundered = makeVictim()

	// The only thing referring to the object now is an integer, so the next
	// two cycles free it and sweep its span.
	runtime.GC()
	runtime.GC()

	%s

	// This cycle marks a slot that is free. The sweep at the end of the next
	// one finds it marked and reports a zombie.
	runtime.GC()
	runtime.GC()

	println("no zombie was reported")
	_ = published
	_ = laundered
}
`

const zombieResurrection = `// Resurrect the integer as a pointer and publish it where the collector
	// will follow it. This is the illegal step.
	published.pointer = unsafe.Pointer(laundered)`

const zombieControl = `// The control does not resurrect anything.
	published.pointer = nil`

func buildZombieProgram(t *testing.T, name, body string) string {
	t.Helper()
	directory := t.TempDir()
	source := filepath.Join(directory, name+".go")
	program := strings.Replace(zombieProgramTemplate, "%s", body, 1)
	require.NoError(t, os.WriteFile(source, []byte(program), 0o644))

	compiler := sharedGOCBinary(t)
	executable := filepath.Join(directory, name)
	compile := exec.Command(compiler, "-o", executable, source)
	output, err := compile.CombinedOutput()
	require.NoErrorf(t, err, "compile %s:\n%s", name, output)
	return executable
}

func runZombieProgram(t *testing.T, executable string) (string, int) {
	t.Helper()
	run := exec.Command(executable)
	run.Env = append(os.Environ(), "GOMAXPROCS=1")
	timer := time.AfterFunc(60*time.Second, func() {
		if run.Process != nil {
			run.Process.Kill()
		}
	})
	output, err := run.CombinedOutput()
	timer.Stop()
	if exit, ok := err.(*exec.ExitError); ok {
		return string(output), exit.ExitCode()
	}
	require.NoErrorf(t, err, "run %s:\n%s", executable, output)
	return string(output), 0
}

func TestSweeperReportsADeliberateZombie(t *testing.T) {
	requireARM64WithCC(t)

	control := buildZombieProgram(t, "zombie-control", zombieControl)
	controlOutput, controlStatus := runZombieProgram(t, control)
	require.Equalf(t, 0, controlStatus,
		"the control must exit cleanly, or this test is only detecting a crash:\n%s", controlOutput)
	require.Contains(t, controlOutput, "no zombie was reported")

	resurrecting := buildZombieProgram(t, "zombie-resurrect", zombieResurrection)
	output, status := runZombieProgram(t, resurrecting)

	require.NotEqualf(t, 0, status,
		"a marked free object must be fatal, but the program exited 0:\n%s", output)
	require.Containsf(t, output, "runtime: marked free object in span",
		"the sweeper did not report the zombie:\n%s", output)
	require.Containsf(t, output, "found pointer to free object",
		"the sweeper reported a zombie without throwing:\n%s", output)
	require.NotContains(t, output, "no zombie was reported",
		"the program ran to completion despite the zombie")

	// reportZombies' own per-object dump is what names which object is the
	// zombie, and on a span with Green Tea inline mark bits it currently names
	// none: sweep calls moveInlineMarks, which resets the inline bits, and then
	// reportZombies reads markBitsForBase, which on such a span returns those
	// same reset bits. The detection above is unaffected -- it reads gcmarkBits,
	// which moveInlineMarks has just filled in -- so this is a gap in
	// attribution rather than in detection.
	//
	// It is logged rather than asserted in either direction: asserting that the
	// dump is blind would write the defect into the suite, and asserting that it
	// names the zombie would fail until it is fixed. ccwork/reportzombies owns
	// the fix; RUNTIME_PLAN section 5.11 records the gap.
	if !strings.Contains(output, " zombie") {
		t.Logf("reportZombies named no zombie in its dump; " +
			"this is the Green Tea inline-mark-bits blind spot, owned by ccwork/reportzombies")
	}
}
