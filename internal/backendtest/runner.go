// Package backendtest builds and runs the code a machine backend generates.
//
// Every backend's tests end the same way: compile a module, link the object into
// an image, run it, look at what came out. That tail was written once per backend
// and then once more per test file -- amd64 had runObj, runObjWith and runAsmSrc,
// arm64 had buildAndRun, runObject and runObjectCC, and no test anywhere ran the
// same check against both. This package holds the tail once, parameterized on the
// machine, so a new backend test says what it wants to observe and nothing about
// temporary directories, linker flags, or emulators.
//
// The part worth reading carefully is the choice of how to execute the image,
// which is this file. Everything else is plumbing.
package backendtest

import (
	"os/exec"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/internal/testenv"
)

// Machine describes a target architecture as running its code requires: which
// host executes its images as native binaries, and what can emulate it elsewhere.
//
// It deliberately says nothing about how an image is built. Compilers, linkers
// and entry stubs vary between test suites for the same machine -- freestanding
// with a hand-written _start, or linked against libc with a C driver -- while the
// question of what can execute the result depends only on the architecture. See
// [Harness] for the build side.
type Machine struct {
	// Name is how the machine is called in skip and failure messages.
	Name string

	// GOARCH is runtime.GOARCH of a host that runs this machine's images
	// directly, with no emulator in between.
	GOARCH string

	// Emulators are user-mode emulators able to run this machine's images on a
	// host of another architecture, most preferred first. They are alternatives
	// for one job, not separate requirements: the first one present is used.
	//
	// Leave it empty for a machine with no emulator available, which makes a
	// cross-architecture host an honest skip rather than a missing tool.
	Emulators []string
}

// X8664 is the 64-bit x86 machine the amd64 backend targets.
var X8664 = Machine{
	Name:      "x86-64",
	GOARCH:    "amd64",
	Emulators: []string{"qemu-x86_64"},
}

// AArch64 is the 64-bit Arm machine the arm64 backend targets.
var AArch64 = Machine{
	Name:      "AArch64",
	GOARCH:    "arm64",
	Emulators: []string{"qemu-aarch64"},
}

// Runner is a resolved way to execute an image built for a [Machine]: either the
// host itself, or the user-mode emulator whose path it holds.
type Runner struct {
	// Emulator is the path to the emulator to exec the image through, or empty
	// when the host runs the image itself.
	Emulator string
}

// Native reports whether the runner executes images directly, without an
// emulator.
func (r Runner) Native() bool {
	return r.Emulator == ""
}

// Describe names how the runner executes an image, for a failure message.
func (r Runner) Describe() string {
	if r.Native() {
		return "natively"
	}
	return "under " + r.Emulator
}

// Command builds the command that runs bin with args through this runner.
func (r Runner) Command(bin string, args ...string) *exec.Cmd {
	if r.Native() {
		return exec.Command(bin, args...)
	}
	emulatorArgs := append([]string{bin}, args...)
	return exec.Command(r.Emulator, emulatorArgs...)
}

// Runner decides how to execute an image built for m on the host running this
// test, skipping when it cannot be executed at all.
//
// An image for the host's own architecture is just a host binary: exec it. An
// emulator is what it takes to reach the machine from a *different*
// architecture -- the arm64 workstations and CI runners this project also builds
// on -- and only there. The distinction is the whole reason this function
// exists: demanding qemu-x86_64 unconditionally is how the amd64 suite came to
// skip all 52 of its execution tests on an x86-64 host and still report ok, and a
// suite that verifies nothing is worse than a red one because it looks green.
//
// Resolving the emulator only on the hosts that need one is also what keeps the
// skip honest in the other direction. A cross-architecture host without the
// emulator genuinely cannot run the image, so it skips -- and fails, naming
// qemu, under CG12_REQUIRE_TOOLS, because there a skip would be a pass nobody
// earned. A machine with no emulator listed at all is a plain skip even under
// CG12_REQUIRE_TOOLS: no tool is missing, this host simply cannot run that code,
// which is not testenv's business.
func (m Machine) Runner(t testing.TB) Runner {
	t.Helper()
	return m.runnerFor(t, runtime.GOARCH)
}

// Native reports whether this host runs m's images directly. Tests that only
// build an image do not need this; it is for a test that wants to say something
// different -- checking a host-specific detail, say -- depending on the answer.
func (m Machine) Native() bool {
	return m.nativeOn(runtime.GOARCH)
}

// nativeOn reports whether a host of architecture goarch runs m's images as its
// own binaries.
func (m Machine) nativeOn(goarch string) bool {
	return goarch == m.GOARCH
}

// runnerFor is [Machine.Runner] with the host architecture as a parameter, so the
// decision can be exercised for hosts the test is not running on. Keep the
// policy here and not in Runner: it is one branch, and it is the branch this
// package exists to get right.
func (m Machine) runnerFor(t testing.TB, goarch string) Runner {
	t.Helper()
	if m.nativeOn(goarch) {
		return Runner{}
	}
	if len(m.Emulators) == 0 {
		t.Skipf("cannot run %s images on %s: no emulator is known for this machine", m.Name, goarch)
		return Runner{}
	}
	// testenv.Tool carries the CG12_REQUIRE_TOOLS contract: a skip for a
	// developer with a partial toolchain, a failure naming the tool for CI.
	return Runner{Emulator: testenv.Tool(t, m.Emulators...)}
}
