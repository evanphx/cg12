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
		// support is the rest of the program: sources the host compiler builds in
		// every arm, for a library that cannot be one translation unit. defines are
		// the -D flags both compilers need to agree on.
		support []string
		defines []string
	}{
		{
			name:    "sqlite",
			library: filepath.Join(benchDir, "sqlite3.c"),
			driver:  filepath.Join("testdata", "behaviour", "sqlite_driver.c"),
			include: benchDir,
			link:    []string{"-lm", "-lpthread", "-ldl"},
		},
		{
			// QuickJS is three programs' worth of C and only quickjs.c goes through
			// the compiler under test: it and libregexp.c each define a static
			// is_digit and a static compute_stack_size with different signatures, so
			// they cannot share a translation unit (gcc rejects that too). The
			// interpreter is all in quickjs.c, which is the part worth checking.
			name:    "quickjs",
			library: filepath.Join(benchDir, "quickjs", "quickjs.c"),
			driver:  filepath.Join("testdata", "behaviour", "quickjs_driver.c"),
			include: filepath.Join(benchDir, "quickjs"),
			link:    []string{"-lm", "-lpthread"},
			support: []string{"libregexp.c", "libunicode.c", "cutils.c", "libbf.c"},
			defines: []string{"_GNU_SOURCE", "CONFIG_BIGNUM", `CONFIG_VERSION="cg12"`},
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
			driverArgs := append(hostDefines(tc.defines), "-c", "-o", driverObj, tc.driver)
			build(t, gcc, append([]string{"-O2", "-I", tc.include}, driverArgs...)...)

			// The support sources are the host compiler's in every arm, so the only
			// thing that differs between arms stays the one object under test.
			extra := []string{driverObj}
			for _, name := range tc.support {
				obj := filepath.Join(dir, name+".o")
				args := append(hostDefines(tc.defines), "-c", "-o", obj, filepath.Join(tc.include, name))
				build(t, gcc, append([]string{"-O2", "-fwrapv", "-I", tc.include}, args...)...)
				extra = append(extra, obj)
			}

			// The reference: the same library, built by the host compiler.
			referenceObj := filepath.Join(dir, "reference.o")
			refArgs := append(hostDefines(tc.defines), "-c", "-o", referenceObj, tc.library)
			build(t, gcc, append([]string{"-O2", "-fwrapv", "-I", tc.include}, refArgs...)...)
			want := runProgram(t, gcc, dir, "reference", referenceObj, extra, tc.link)
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
						Defines:     tc.defines,
					})
					require.NoError(t, err)
					if optimize {
						opt.OptimizeModule(module)
					}
					// PIC because the link below is the host default, a PIE.
					code, err := arm64.CompileObjectWith(module, arm64.Options{PIC: true})
					require.NoError(t, err)

					libraryObj := filepath.Join(dir, name+".o")
					require.NoError(t, os.WriteFile(libraryObj, code, 0o644))
					got := runProgram(t, gcc, dir, name, libraryObj, extra, tc.link)
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
// The link is the host toolchain's default, which here is a position-independent
// executable. That is deliberate: it is the link every other compiler on this
// platform gets, and it only works because the backend reaches external data
// through the GOT (arm64.Options.PIC). Before that it failed outright on Lua --
// "unresolvable R_AARCH64_ADR_PREL_PG_HI21 relocation against symbol `stdin'" --
// so this arm is what keeps the GOT path honest on a real program.
// hostDefines turns the case's -D list into host compiler flags. Both compilers
// must see the same ones or they are not building the same program.
func hostDefines(defines []string) []string {
	var args []string
	for _, d := range defines {
		args = append(args, "-D"+d)
	}
	return args
}

func runProgram(t *testing.T, gcc, dir, name, libraryObj string, others []string, link []string) string {
	t.Helper()
	bin := filepath.Join(dir, name)
	args := append(append([]string{"-o", bin, libraryObj}, others...), link...)
	out, err := exec.Command(gcc, args...).CombinedOutput()
	require.NoErrorf(t, err, "link %s: %s", name, out)

	command := exec.Command(bin)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	require.NoErrorf(t, command.Run(), "run %s: %s%s", name, stdout.String(), stderr.String())
	return stdout.String()
}
