package cc_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// TestBehaviourOnRealPrograms compiles a large real C program with cg12, runs
// it, and requires it to agree with the same program built by the host
// compiler.
//
// It exists because the checks this compiler had were the wrong shape. The
// rubric runs real programs but they are tiny; scripts/cc-identity-check.sh
// covers SQLite and Lua but only asks whether the emitted object CHANGED, which
// is a question about stability, not correctness. Both answered yes for a long
// time while cg12 was miscompiling SQLite: a wrong b-tree cell count that
// corrupted any table over one page, and an alloca in an unreachable block that
// crashed the parser under the optimiser. Neither could have been caught by
// comparing bytes to yesterday's bytes. Both are obvious the moment the program
// runs.
//
// The programs are fetched rather than committed (scripts/fetch-bench-c.sh), so
// this skips when they are absent rather than failing.
func TestBehaviourOnRealPrograms(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a 250k-line program three times")
	}
	gcc := testenv.Tool(t, "gcc")

	benchDir, err := filepath.Abs(filepath.Join("testdata", "bench"))
	require.NoError(t, err)
	cases := []struct {
		name string
		// library is the big program under test, driver the small harness that
		// exercises it. include is what the driver needs on its include path.
		library string
		driver  string
		include string
		link    []string
	}{
		{
			name:    "sqlite",
			library: filepath.Join(benchDir, "sqlite3.c"),
			driver:  filepath.Join("testdata", "behaviour", "sqlite_driver.c"),
			include: benchDir,
			link:    []string{"-lm", "-lpthread", "-ldl"},
		},
		{
			name:    "lua",
			library: filepath.Join(benchDir, "lua", "onelua.c"),
			driver:  filepath.Join("testdata", "behaviour", "lua_driver.c"),
			include: filepath.Join(benchDir, "lua"),
			link:    []string{"-lm"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source, err := os.ReadFile(tc.library)
			if os.IsNotExist(err) {
				t.Skipf("%s is not present; run scripts/fetch-bench-c.sh", tc.library)
			}
			require.NoError(t, err)

			dir := t.TempDir()

			// The driver is built by the host compiler in every arm, so the only
			// thing that differs between them is the object the compiler under test
			// produced for the library.
			driverObj := filepath.Join(dir, "driver.o")
			build(t, gcc, "-O2", "-I", tc.include, "-c", "-o", driverObj, tc.driver)

			// The reference: the same library, built by the host compiler.
			referenceObj := filepath.Join(dir, "reference.o")
			build(t, gcc, "-O2", "-I", tc.include, "-c", "-o", referenceObj, tc.library)
			want := runProgram(t, gcc, dir, "reference", referenceObj, driverObj, tc.link)
			require.NotEmpty(t, want, "the reference build produced no output")
			t.Logf("reference: %s", want)

			// cg12, unoptimised and optimised. Both must agree with the reference:
			// the optimiser is not allowed to change what the program computes, and
			// the front end is not allowed to disagree with the host compiler about
			// what the program means.
			for _, optimize := range []bool{false, true} {
				name := "raw"
				if optimize {
					name = "opt"
				}
				t.Run(name, func(t *testing.T) {
					module, err := cc.CompileWith(tc.library, string(source), cc.Options{
						IncludeDirs: []string{tc.include},
					})
					require.NoError(t, err)
					if optimize {
						opt.OptimizeModule(module)
					}
					code, err := arm64.CompileObject(module)
					require.NoError(t, err)

					libraryObj := filepath.Join(dir, name+".o")
					require.NoError(t, os.WriteFile(libraryObj, code, 0o644))
					got := runProgram(t, gcc, dir, name, libraryObj, driverObj, tc.link)
					require.Equal(t, want, got)
				})
			}
		})
	}
}

func build(t *testing.T, gcc string, args ...string) {
	t.Helper()
	out, err := exec.Command(gcc, args...).CombinedOutput()
	require.NoErrorf(t, err, "%s %v: %s", gcc, args, out)
}

// runProgram links one library object with the shared driver and runs it.
//
// -no-pie is required: cg12 emits absolute relocations against external data
// (stdin and friends), which a position-independent executable cannot use. It is
// passed to every arm so the reference is linked the same way.
func runProgram(t *testing.T, gcc, dir, name, libraryObj, driverObj string, link []string) string {
	t.Helper()
	bin := filepath.Join(dir, name)
	args := append([]string{"-no-pie", "-o", bin, libraryObj, driverObj}, link...)
	out, err := exec.Command(gcc, args...).CombinedOutput()
	require.NoErrorf(t, err, "link %s: %s", name, out)

	command := exec.Command(bin)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	require.NoErrorf(t, command.Run(), "run %s: %s%s", name, stdout.String(), stderr.String())
	return stdout.String()
}
