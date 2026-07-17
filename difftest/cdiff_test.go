package difftest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// The C corpus is checked differentially against gcc rather than against
// hand-written expectations. Both are compiled from the same source and run, and
// their output has to match.
//
// This is a different question from the rubric's. The rubric asks "does this do
// what we thought it should", which cannot catch a case where we thought wrong.
// gcc is the arbiter of what the C means, so a disagreement is a bug in us --
// and every bug this found was one nobody had thought to write an expectation
// for.
//
// The programs must therefore be well-defined C: implementation-defined is fine
// (matching gcc is the goal, and cg12 targets the same machine), but undefined
// behaviour is not, since gcc is then entitled to any answer at all.

// cDiffCase is one program, run through both compilers.
type cDiffCase struct {
	name string
	src  string
}

var cDiffCases = []cDiffCase{
	{"constant-initializers", `
#include <stdio.h>
double  di = 1;          /* an int literal initializing a double */
double  dd = 2.5;
float   fi = 3;
int     id = 4;          /* a float literal initializing an int */
long    li = 5;
short   si = 6;
char    ci = 7;
unsigned ui = 8;
struct S { double x; double y; } s = {9, 10.5};
struct M { int a; double b; char c; } m = {11, 12.5, 13};
static double sd = 14;   /* the static-local path */
int main(void){
  printf("%g %g %g %d %ld %d %d %u %g %g %d %g %d %g\n",
         di, dd, (double)fi, id, li, (int)si, (int)ci, ui,
         s.x, s.y, m.a, m.b, (int)m.c, sd);
  return 0;
}`},

	{"narrowing-casts", `
#include <stdio.h>
int main(void){
  int big = 300, huge = 70000, neg = -1;
  printf("%d %d %d %d %d %d\n",
         (int)(unsigned char)big,
         (int)(unsigned char)huge,
         (int)(unsigned short)huge,
         (int)(unsigned short)neg,
         (int)(signed char)big,
         (int)(short)huge);
  return 0;
}`},

	{"integer-promotion", `
#include <stdio.h>
int main(void){
  unsigned char uc = 200;
  unsigned short us = 40000;
  signed char sc = -1;
  /* Both operands promote to int, so these are SIGNED comparisons. */
  printf("%d %d %d %d %d %d\n",
         uc < -1, uc > -1, us < -1, us > -1, sc < 1, sc == -1);
  return 0;
}`},

	{"overflow-builtins", `
#include <stdio.h>
int main(void){
  unsigned ur = 0; int ir = 0; long lr = 0;
  /* Unsigned: wraps at the top, never negative. */
  printf("%d %d %d %d %d\n",
         __builtin_add_overflow(4000000000u, 1000000000u, &ur),
         __builtin_sub_overflow(1u, 2u, &ur),
         __builtin_mul_overflow(3000000000u, 2u, &ur),
         __builtin_add_overflow(1u, 2u, &ur),
         __builtin_mul_overflow(100u, 3u, &ur));
  /* Signed: including MIN * -1, which the divide-back check alone misses. */
  printf("%d %d %d %d %d %d\n",
         __builtin_add_overflow(2000000000, 2000000000, &ir),
         __builtin_sub_overflow(-2000000000, 2000000000, &ir),
         __builtin_mul_overflow(-2147483647-1, -1, &ir),
         __builtin_mul_overflow(65536, 65536, &ir),
         __builtin_add_overflow(1, 2, &ir),
         __builtin_add_overflow(1L, 2L, &lr));
  /* The wrapped value is stored through the pointer either way. */
  __builtin_add_overflow(4000000000u, 1000000000u, &ur);
  __builtin_sub_overflow(1u, 2u, &ur);
  unsigned w1 = ur;
  __builtin_add_overflow(4000000000u, 1000000000u, &ur);
  printf("%u %u\n", w1, ur);
  return 0;
}`},

	{"subword-arithmetic", `
#include <stdio.h>
int main(void){
  unsigned char a = 250, b = 10;
  unsigned short c = 65530, d = 10;
  signed char e = 120, f = 10;
  printf("%d %d %d %d %d\n", a + b, (int)(unsigned char)(a + b),
         c + d, (int)(unsigned short)(c + d), e + f);
  return 0;
}`},

	{"struct-layout", `
#include <stdio.h>
struct A { char c; int i; };
struct B { int i; char c; };
struct C { double d; char c; };
struct D { char a; short b; char c; };
int main(void){
  printf("%zu %zu %zu %zu\n", sizeof(struct A), sizeof(struct B),
         sizeof(struct C), sizeof(struct D));
  printf("%zu %zu %zu\n", __builtin_offsetof(struct A, i),
         __builtin_offsetof(struct B, c), __builtin_offsetof(struct D, c));
  return 0;
}`},
}

func TestCDiffAgainstGCC(t *testing.T) {
	gcc := cDiffTool(t)
	for _, c := range cDiffCases {
		t.Run(c.name, func(t *testing.T) {
			want := gccOutput(t, gcc, c.src)
			// Both optimization levels, so a disagreement introduced by a pass is
			// distinguishable from one the frontend produced.
			for _, o := range []bool{false, true} {
				level := "raw"
				if o {
					level = "opt"
				}
				t.Run(level, func(t *testing.T) {
					require.Equal(t, want, cg12Output(t, gcc, c.src, o),
						"cg12 disagrees with gcc")
				})
			}
		})
	}
}

// gccOutput compiles and runs the program with gcc: the reference answer.
func gccOutput(t *testing.T, gcc, src string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ref.c")
	bin := filepath.Join(dir, "ref")
	require.NoError(t, os.WriteFile(path, []byte(src), 0o644))
	out, err := exec.Command(gcc, "-w", "-O0", "-o", bin, path).CombinedOutput()
	require.NoErrorf(t, err, "gcc could not compile the reference: %s", out)
	return runBin(t, bin)
}

// cg12Output compiles the program with cg12 and links it against libc with gcc.
func cg12Output(t *testing.T, gcc, src string, optimize bool) string {
	t.Helper()
	m, err := cc.Compile("prog.c", src)
	require.NoError(t, err)
	if optimize {
		opt.OptimizeModule(m)
	}
	code, err := arm64.CompileObject(m)
	require.NoError(t, err)

	dir := t.TempDir()
	obj := filepath.Join(dir, "prog.o")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(obj, code, 0o644))
	out, err := exec.Command(gcc, "-o", bin, obj).CombinedOutput()
	require.NoErrorf(t, err, "link: %s", out)
	return runBin(t, bin)
}

func runBin(t *testing.T, bin string) string {
	t.Helper()
	cmd := exec.Command(bin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("run %s: %v", bin, err)
		}
	}
	return stdout.String()
}

// cDiffTool finds a native gcc. The comparison is only meaningful when gcc
// targets the same machine cg12 compiles for, so this does not fall back to a
// cross-compiler.
func cDiffTool(t *testing.T) string {
	t.Helper()
	if runtime.GOARCH != "arm64" {
		t.Skip("cc targets arm64, so only an arm64 host's gcc is the right arbiter")
	}
	return testenv.Tool(t, "gcc")
}
