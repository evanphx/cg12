package backendtest

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// These tests are about the one decision this package exists to make -- run the
// image here, run it under an emulator, or skip and say so -- and they check it
// without a backend, a toolchain, or an image, because the decision does not
// depend on any of those.

// A machine whose GOARCH no host has and whose emulator no PATH has, for
// exercising the branches this host does not take. The name is deliberately
// implausible: if some future PATH really has it, the tests using it fail loudly
// rather than quietly checking the wrong branch.
var absentMachine = Machine{
	Name:      "absent-arch",
	GOARCH:    "absentarch",
	Emulators: []string{"cg12-no-such-emulator"},
}

// A machine nothing can emulate, which is how a backend says "here or nowhere".
var unemulatableMachine = Machine{
	Name:   "unemulatable-arch",
	GOARCH: "absentarch",
}

func TestNativeOnMatchingHost(t *testing.T) {
	if !X8664.nativeOn("amd64") {
		t.Error("x86-64 images are native on an amd64 host")
	}
	if X8664.nativeOn("arm64") {
		t.Error("x86-64 images are not native on an arm64 host")
	}
	if !AArch64.nativeOn("arm64") {
		t.Error("AArch64 images are native on an arm64 host")
	}
	if AArch64.nativeOn("amd64") {
		t.Error("AArch64 images are not native on an amd64 host")
	}
}

// Machine.Native must agree with the host this test is running on -- the check
// that catches a Machine declared with the wrong GOARCH, which would send every
// test on that backend down the emulator path.
func TestNativeAgreesWithHost(t *testing.T) {
	if X8664.Native() != (runtime.GOARCH == "amd64") {
		t.Errorf("X8664.Native() = %v on GOARCH %s", X8664.Native(), runtime.GOARCH)
	}
	if AArch64.Native() != (runtime.GOARCH == "arm64") {
		t.Errorf("AArch64.Native() = %v on GOARCH %s", AArch64.Native(), runtime.GOARCH)
	}
}

// The native branch resolves no tool at all: an image for the host's own
// architecture is a host binary, and needing qemu to be installed before running
// one is exactly the mistake that silenced the amd64 suite.
func TestRunnerNativeNeedsNoEmulator(t *testing.T) {
	runner := X8664.runnerFor(t, "amd64")
	if !runner.Native() {
		t.Fatalf("runner for an amd64 host is not native: emulator %q", runner.Emulator)
	}
	if runner.Emulator != "" {
		t.Errorf("native runner names an emulator: %q", runner.Emulator)
	}
	if runner.Describe() != "natively" {
		t.Errorf("Describe() = %q, want %q", runner.Describe(), "natively")
	}
}

// The emulator branch: a host of another architecture resolves the emulator, and
// the command it builds runs the image through it.
func TestRunnerCrossUsesEmulator(t *testing.T) {
	// A tool certain to be on PATH stands in for qemu, so this exercises the
	// cross-architecture branch on any host.
	machine := Machine{Name: "stand-in", GOARCH: "absentarch", Emulators: []string{"go"}}
	runner := machine.runnerFor(t, runtime.GOARCH)
	if runner.Native() {
		t.Fatal("runner for a foreign architecture claims to be native")
	}
	if !strings.HasSuffix(runner.Emulator, "go") {
		t.Errorf("emulator = %q, want a path ending in go", runner.Emulator)
	}
	if !strings.Contains(runner.Describe(), runner.Emulator) {
		t.Errorf("Describe() = %q, want it to name the emulator", runner.Describe())
	}
}

// Command puts the emulator first and the image after it, with the image's own
// arguments following: qemu-x86_64 /path/image arg...
func TestCommandShape(t *testing.T) {
	native := Runner{}.Command("/path/image", "a", "b")
	if native.Path != "/path/image" {
		t.Errorf("native command path = %q, want the image", native.Path)
	}
	if got := strings.Join(native.Args, " "); got != "/path/image a b" {
		t.Errorf("native command args = %q", got)
	}

	emulated := Runner{Emulator: "/usr/bin/qemu-x86_64"}.Command("/path/image", "a", "b")
	if emulated.Path != "/usr/bin/qemu-x86_64" {
		t.Errorf("emulated command path = %q, want the emulator", emulated.Path)
	}
	if got := strings.Join(emulated.Args, " "); got != "/usr/bin/qemu-x86_64 /path/image a b" {
		t.Errorf("emulated command args = %q", got)
	}
}

// The honesty contract, checked as a test observes it rather than as the code
// spells it: a missing emulator skips for a developer and fails for CI. Both are
// terminal for the test that hits them, so they are checked in a child process
// running one test each -- the only way to see a skip or a failure from outside.
func TestMissingEmulatorSkipsAndFailsUnderRequireTools(t *testing.T) {
	cases := []struct {
		name string
		// test is the child test to run, one of the helpers below.
		test string
		// requireTools sets CG12_REQUIRE_TOOLS for the child.
		requireTools bool
		// wantOutcome is the child's verdict line prefix.
		wantOutcome string
		// wantMessage must appear in the child's output, so the person reading a
		// CI log learns which tool was missing and what went unchecked.
		wantMessage []string
	}{
		{
			name:        "missing emulator skips",
			test:        "TestChildMissingEmulator",
			wantOutcome: "--- SKIP",
			wantMessage: []string{"cg12-no-such-emulator", "not available"},
		},
		{
			name:         "missing emulator fails under CG12_REQUIRE_TOOLS",
			test:         "TestChildMissingEmulator",
			requireTools: true,
			wantOutcome:  "--- FAIL",
			wantMessage:  []string{"CG12_REQUIRE_TOOLS", "cg12-no-such-emulator"},
		},
		{
			// No emulator exists for the machine at all, so nothing is missing
			// from PATH and there is nothing for CG12_REQUIRE_TOOLS to demand.
			// This one stays a skip even in CI.
			name:         "no emulator known is a plain skip even under CG12_REQUIRE_TOOLS",
			test:         "TestChildUnemulatableMachine",
			requireTools: true,
			wantOutcome:  "--- SKIP",
			wantMessage:  []string{"cannot run unemulatable-arch images on"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := runChildTest(t, tc.test, tc.requireTools)
			if !strings.Contains(out, tc.wantOutcome) {
				t.Errorf("child did not report %s:\n%s", tc.wantOutcome, out)
			}
			for _, want := range tc.wantMessage {
				if !strings.Contains(out, want) {
					t.Errorf("child output does not mention %q:\n%s", want, out)
				}
			}
		})
	}
}

// runChildTest re-runs this test binary for the single named test and returns its
// verbose output. The child's own verdict is the thing under test, so a non-zero
// exit is expected and not reported as an error here.
func runChildTest(t *testing.T, name string, requireTools bool) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	if requireTools {
		cmd.Env = append(cmd.Env, "CG12_REQUIRE_TOOLS=1")
	} else {
		// The parent may itself be running under CG12_REQUIRE_TOOLS, and the
		// child must not inherit it for the developer-toolchain case.
		cmd.Env = append(cmd.Env, "CG12_REQUIRE_TOOLS=0")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("run child test %s: %v\n%s", name, err, out)
		}
	}
	return string(out)
}

// childEnv marks a child process spawned by runChildTest. Without it the helper
// tests below do nothing and pass, so they never appear as skips of their own in
// a normal run of this package.
const childEnv = "CG12_BACKENDTEST_CHILD"

// TestChildMissingEmulator asks for a runner on a host that needs an emulator
// which is not installed. It is a helper for
// TestMissingEmulatorSkipsAndFailsUnderRequireTools, not a test of its own.
func TestChildMissingEmulator(t *testing.T) {
	if os.Getenv(childEnv) != "1" {
		return
	}
	runner := absentMachine.runnerFor(t, runtime.GOARCH)
	t.Fatalf("expected a skip or a failure, got runner %+v", runner)
}

// TestChildUnemulatableMachine asks for a runner for a machine no emulator
// covers. Helper, as above.
func TestChildUnemulatableMachine(t *testing.T) {
	if os.Getenv(childEnv) != "1" {
		return
	}
	runner := unemulatableMachine.runnerFor(t, runtime.GOARCH)
	t.Fatalf("expected a skip, got runner %+v", runner)
}
