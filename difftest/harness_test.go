// Package difftest is a differential correctness harness. Each corpus program
// (QBE-compatible textual IL plus a C driver) is compiled by cg12 both without
// and with optimization and run; the two outputs must agree with the expected
// result, so the optimizer is continuously checked to be semantics-preserving.
//
// When a QBE binary is available (on PATH or via the CG12_QBE environment
// variable) the same IL is also compiled by QBE and run, giving a true
// differential comparison against the reference implementation.
package difftest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// Case is one differential test: IL source, a C driver that prints a result,
// and the expected program output.
type Case struct {
	Name string
	IL   string
	Main string
	Want string
}

func assembler(t *testing.T) string {
	if p, err := exec.LookPath("aarch64-linux-gnu-gcc"); err == nil {
		return p
	}
	if runtime.GOARCH == "arm64" {
		for _, c := range []string{"cc", "gcc", "clang"} {
			if p, err := exec.LookPath(c); err == nil {
				return p
			}
		}
	}
	t.Skip("no AArch64 assembler available")
	return ""
}

// qbePath returns a QBE binary if one is configured.
func qbePath() (string, bool) {
	if p := os.Getenv("CG12_QBE"); p != "" {
		return p, true
	}
	if p, err := exec.LookPath("qbe"); err == nil {
		return p, true
	}
	return "", false
}

// run compiles the case through cg12 (both optimization levels) and QBE (if
// present) and requires every path to produce Want.
func (c Case) run(t *testing.T) {
	cc := assembler(t)

	require.Equal(t, c.Want, c.cg12(t, cc, false), "%s: cg12 unoptimized", c.Name)
	require.Equal(t, c.Want, c.cg12(t, cc, true), "%s: cg12 optimized", c.Name)

	if qbe, ok := qbePath(); ok {
		require.Equal(t, c.Want, c.qbe(t, cc, qbe), "%s: QBE reference", c.Name)
	}
}

// cg12 parses, optionally optimizes, compiles, links, and runs.
func (c Case) cg12(t *testing.T, cc string, optimize bool) string {
	m, err := parse.Parse(c.IL)
	require.NoErrorf(t, err, "%s: parse", c.Name)
	if optimize {
		opt.OptimizeModule(m)
	}
	asm, err := arm64.CompileModule(m)
	require.NoErrorf(t, err, "%s: compile", c.Name)
	return linkRun(t, cc, asm, c.Main)
}

// qbe compiles the IL with the reference QBE, links, and runs.
func (c Case) qbe(t *testing.T, cc, qbe string) string {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.ssa")
	out := filepath.Join(dir, "qbe.s")
	require.NoError(t, os.WriteFile(src, []byte(c.IL), 0o644))
	if o, err := exec.Command(qbe, "-o", out, src).CombinedOutput(); err != nil {
		t.Fatalf("%s: qbe failed: %s", c.Name, o)
	}
	asm, err := os.ReadFile(out)
	require.NoError(t, err)
	return linkRun(t, cc, string(asm), c.Main)
}

// linkRun assembles asm with the C driver, runs the program, and returns stdout.
func linkRun(t *testing.T, cc, asm, cmain string) string {
	dir := t.TempDir()
	asmPath := filepath.Join(dir, "o.s")
	cPath := filepath.Join(dir, "main.c")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(asmPath, []byte(asm), 0o644))
	require.NoError(t, os.WriteFile(cPath, []byte(cmain), 0o644))

	if o, err := exec.Command(cc, "-o", bin, asmPath, cPath).CombinedOutput(); err != nil {
		t.Fatalf("assemble/link failed: %s\n--- asm ---\n%s", o, asm)
	}
	cmd := exec.Command(bin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("run: %v", err)
		}
	}
	return stdout.String()
}

func TestCorpus(t *testing.T) {
	if _, ok := qbePath(); ok {
		t.Log("QBE reference oracle enabled")
	} else {
		t.Log("QBE not found; comparing cg12 optimized vs unoptimized only")
	}
	for _, c := range corpus {
		t.Run(c.Name, c.run)
	}
}
