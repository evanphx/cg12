// Package testenv reports which host tools the tests may use.
//
// Much of this suite checks cg12 against something outside it: an assembler
// agrees our bytes are the instruction we meant, a linker and a loader agree our
// image runs, gcc agrees our C means what C means. Those are the checks worth
// most, and each is skipped when its tool is absent -- so a green run on a bare
// machine can mean almost nothing was verified. Breaking llvm-mc alone silently
// takes 21 of the x64 encoder's 26 tests with it, and the package still says ok.
//
// Skipping is right for a developer with a partial toolchain. It is wrong for CI,
// which must not report a pass it did not earn. Set CG12_REQUIRE_TOOLS=1 and a
// missing tool becomes a failure naming it.
//
// A test that cannot run for a reason other than a missing tool -- an arm64
// binary on an x86 host -- is a genuine skip, and not this package's business.
package testenv

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// required reports whether the environment insists every tool be present.
func required() bool {
	v := os.Getenv("CG12_REQUIRE_TOOLS")
	return v != "" && v != "0" && !strings.EqualFold(v, "false")
}

// Tool returns the path to the first of names found on PATH. When none is, it
// skips -- or, under CG12_REQUIRE_TOOLS, fails: a tool missing in CI means the
// check that tool performs did not happen, which a skip reports as success.
//
// Several names mean alternatives for one job (a cross assembler or the native
// one), not several tools.
func Tool(t testing.TB, names ...string) string {
	t.Helper()
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	missing := strings.Join(names, " or ")
	if required() {
		t.Fatalf("CG12_REQUIRE_TOOLS is set and %s is not on PATH: this test's check cannot run", missing)
	}
	t.Skipf("%s not available", missing)
	return ""
}

// Have reports whether a tool is present, without skipping. Use it where a tool
// makes a test stronger but the test is worth running regardless -- an oracle
// that confirms a result already checked another way.
func Have(names ...string) (string, bool) {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p, true
		}
	}
	return "", false
}

// X86Sysroot returns a directory holding an x86-64 loader and libc, for running
// a dynamically linked x86-64 image under qemu-x86_64 -L. It skips when there is
// none.
//
// A dynamic image is loaded by an interpreter, so running one needs that
// target's ld.so and its libraries -- which a machine of another architecture
// has no reason to have. Without it the amd64 dynamic path can only be inspected,
// never run, and the PLT stubs and GOT initialisation are hand-encoded bytes that
// a structural check cannot tell apart from wrong ones.
//
// Set CG12_X86_SYSROOT, or install a cross libc (Debian/Ubuntu:
// libc6-amd64-cross, which lands in /usr/x86_64-linux-gnu).
func X86Sysroot(t testing.TB) string {
	t.Helper()
	if p := os.Getenv("CG12_X86_SYSROOT"); p != "" {
		return p
	}
	for _, p := range []string{"/usr/x86_64-linux-gnu"} {
		if fi, err := os.Stat(p + "/lib/ld-linux-x86-64.so.2"); err == nil && !fi.IsDir() {
			return p
		}
	}
	if required() {
		t.Fatal("CG12_REQUIRE_TOOLS is set and there is no x86-64 sysroot: " +
			"the dynamic image cannot be run, only inspected. Set CG12_X86_SYSROOT, " +
			"or install libc6-amd64-cross.")
	}
	t.Skip("no x86-64 sysroot (set CG12_X86_SYSROOT, or install libc6-amd64-cross)")
	return ""
}
