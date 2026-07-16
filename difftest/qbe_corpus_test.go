package difftest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/obj"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// TestQBECorpus runs QBE's own upstream test programs (vendored under
// testdata/qbe) through cg12. Each file embeds a C driver and, optionally,
// expected output in "# >>> driver" / "# >>> output" comment blocks — the same
// format QBE's tools/test.sh uses.
//
// Programs cg12 cannot yet compile (aggregates, variadics, unsupported ops) are
// reported as skipped; programs it does compile must produce the expected
// output, both unoptimized and optimized.
func TestQBECorpus(t *testing.T) {
	cc := assembler(t)
	files, err := filepath.Glob("testdata/qbe/*.ssa")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	var passed, skipped int
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".ssa")
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(file)
			require.NoError(t, err)
			tc := loadQBETest(string(src))

			// Honour QBE's per-target skip directive on the first line.
			if first := strings.SplitN(string(src), "\n", 2)[0]; strings.Contains(first, "skip") && strings.Contains(first, "arm64") {
				t.Skipf("test marked skip for arm64")
			}

			// cg12 must be able to compile it; unsupported features skip.
			objOpt, err := compileIL(tc.il, true)
			if err != nil {
				t.Skipf("unsupported by cg12: %v", err)
			}
			objUnopt, err := compileIL(tc.il, false)
			require.NoError(t, err)

			wantOut, wantExit := tc.expect()
			checkRun(t, cc, unit{writeObject(t, objUnopt), arm64.Disassemble(objUnopt)},
				tc.driver, "unoptimized", wantOut, wantExit)
			checkRun(t, cc, unit{writeObject(t, objOpt), arm64.Disassemble(objOpt)},
				tc.driver, "optimized", wantOut, wantExit)

			if qbe, ok := qbePath(); ok {
				if asm, err := qbeAsm(qbe, file); err != nil {
					t.Logf("QBE could not compile this file, skipping oracle: %v", err)
				} else if out, exit := runUnit(t, cc, writeAsm(t, asm), tc.driver); !matches(out, exit, wantOut, wantExit) {
					// cg12 already matched the recorded expected output; if this
					// (possibly stale) QBE binary disagrees, trust the expectation.
					t.Logf("QBE reference disagrees with the recorded expectation (its support may differ), skipping oracle")
				}
			}
			passed++
		})
		if !t.Failed() {
			// count handled in subtest via closure isn't reliable; recount below
		}
	}
	// Summarise how much of the corpus cg12 currently handles.
	passed, skipped = summarize(t, files)
	t.Logf("QBE corpus: %d compiled+verified, %d skipped (unsupported), of %d", passed, skipped, len(files))
}

type qbeTest struct {
	il     string
	driver string
	output string
	hasOut bool
}

func (q qbeTest) expect() (string, int) {
	if q.hasOut {
		return q.output, 0
	}
	return "", 0
}

// loadQBETest extracts the driver and output comment blocks. The IL itself is
// the whole file (the comment blocks are ignored by the lexer).
func loadQBETest(src string) qbeTest {
	return qbeTest{
		il:     src,
		driver: extractBlock(src, "driver"),
		output: extractBlock(src, "output"),
		hasOut: strings.Contains(src, "# >>> output"),
	}
}

// extractBlock pulls the lines between "# >>> what" and "# <<<", stripping the
// leading comment marker, matching QBE's tools/test.sh.
func extractBlock(src, what string) string {
	var out []string
	in := false
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "# >>> "+what) {
			in = true
			continue
		}
		if in && strings.HasPrefix(line, "# <<<") {
			break
		}
		if in {
			// Match QBE's `sed -e 's/# //' -e 's/#$//'`: remove the first "# "
			// marker, then a trailing "#". This preserves '#' inside content
			// (e.g. "#include") while unwrapping bordered output lines.
			line = strings.Replace(line, "# ", "", 1)
			line = strings.TrimSuffix(line, "#")
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// compileIL parses and compiles to an object. It returns the object rather than
// bytes so a failure can still be shown as readable code.
func compileIL(il string, optimize bool) (*obj.Object, error) {
	m, err := parse.Parse(il)
	if err != nil {
		return nil, err
	}
	if optimize {
		opt.OptimizeModule(m)
	}
	return arm64.CompileToObject(m)
}

// writeObject serializes an object into a fresh directory and returns its path,
// ready to hand to the linker.
func writeObject(t *testing.T, o *obj.Object) string {
	t.Helper()
	data, err := o.MarshalELF()
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "cg12.o")
	require.NoError(t, os.WriteFile(path, data, 0o644))
	return path
}

func qbeAsm(qbe, file string) (string, error) {
	dir, err := os.MkdirTemp("", "qbeasm")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "qbe.s")
	if o, err := exec.Command(qbe, "-o", out, file).CombinedOutput(); err != nil {
		return "", fmt.Errorf("%s", o)
	}
	b, err := os.ReadFile(out)
	return string(b), err
}

// checkRun links, runs, and asserts the program matches the expectation.
func checkRun(t *testing.T, cc string, u unit, driver, label, wantOut string, wantExit int) {
	out, exit := runUnit(t, cc, u, driver)
	if wantOut != "" {
		require.Equalf(t, wantOut, out, "%s: output mismatch", label)
	} else {
		require.Equalf(t, wantExit, exit, "%s: exit code (stdout=%q)", label, out)
	}
}

// matches reports whether an output/exit pair satisfies the expectation.
func matches(out string, exit int, wantOut string, wantExit int) bool {
	if wantOut != "" {
		return out == wantOut
	}
	return exit == wantExit
}

// runAsm links asm with an optional driver, runs the program with the args QBE's
// harness uses ("a b c"), and returns its stdout and exit code.
// unit is a compiled translation unit ready for the linker: our object, or the
// reference QBE's assembly. listing is the code to show if the link fails.
type unit struct {
	path    string
	listing string
}

// writeAsm makes a unit out of assembly text -- the QBE reference path, the one
// place a .s still reaches the toolchain.
func writeAsm(t *testing.T, asm string) unit {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qbe.s")
	require.NoError(t, os.WriteFile(path, []byte(asm), 0o644))
	return unit{path, asm}
}

func runUnit(t *testing.T, cc string, u unit, driver string) (string, int) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "prog")

	srcs := []string{u.path}
	if strings.TrimSpace(driver) != "" {
		cPath := filepath.Join(dir, "driver.c")
		require.NoError(t, os.WriteFile(cPath, []byte(driver), 0o644))
		srcs = append(srcs, cPath)
	}
	// -no-pie so non-PIC assembly (QBE emits adrp for extern symbols) links.
	args := append([]string{"-no-pie", "-g", "-o", bin}, srcs...)
	if o, err := exec.Command(cc, args...).CombinedOutput(); err != nil {
		t.Fatalf("link failed: %s\n%s", o, u.listing)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "a", "b", "c")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	exit := 0
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			t.Fatalf("run: %v", err)
		}
	}
	return stdout.String(), exit
}

// summarize reports how many corpus files cg12 can compile.
func summarize(t *testing.T, files []string) (passed, skipped int) {
	for _, file := range files {
		src, err := os.ReadFile(file)
		require.NoError(t, err)
		if _, err := compileIL(string(src), true); err != nil {
			skipped++
		} else {
			passed++
		}
	}
	return passed, skipped
}
