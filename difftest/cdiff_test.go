package difftest

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/evanphx/cg12/analysis"
	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/ir"
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

// readTestdata loads a committed C program from testdata. The jump-threading
// reproducers live as real files (not inline strings) because they are also the
// objdump targets for the instruction-count acceptance checks, so the file on
// disk is the single source of truth for both the differential run and objdump.
func readTestdata(name string) string {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		panic(err)
	}
	return string(b)
}

var cDiffCases = []cDiffCase{
	{"mini-and-fastpath", readTestdata("mini_and.c")},
	{"sentinel-comp", readTestdata("comp.c")},

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

	{"statement-expressions", `
#include <stdio.h>
/* A goto wholly within a statement expression: gcc compiles this, and the
   label needs a block before the goto that targets it is generated. */
static int pick(int a){
  return ({ int t = a; if (t > 0) goto pos; t = -t; pos: t + 1; });
}
static int nested(int a){
  return ({ int u = ({ int v = a; if (v > 10) goto big; v; big: v * 2; }); u + 1; });
}
static int loops(int n){
  int s = 0;
  for (int i = 0; i < n; i++) s += ({ int t = i; if (t % 2) goto odd; t; odd: t * 10; });
  return s;
}
int main(void){
  printf("%d %d %d %d %d\n", pick(5), pick(-5), nested(20), nested(3), loops(4));
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

	// An over-aligned object must actually land on its alignment. cg12's alloc op
	// names its alignment in the opcode and stops at 16, and the stack pointer is
	// only 16-aligned besides, so anything stricter has to be established at run
	// time rather than by the frame layout. Asking where it landed is the only way
	// to tell: a wrongly-aligned buffer reads and writes perfectly well.
	{"over-aligned", `
#include <stdio.h>
char g64[64] __attribute__((aligned(64)));
int main(void){
  char l64[64] __attribute__((aligned(64)));
  char l32[32] __attribute__((aligned(32)));
  char l16[16] __attribute__((aligned(16)));
  char l8[8]   __attribute__((aligned(8)));
  printf("%d %d %d %d %d\n",
         (int)((unsigned long)g64 % 64), (int)((unsigned long)l64 % 64),
         (int)((unsigned long)l32 % 32), (int)((unsigned long)l16 % 16),
         (int)((unsigned long)l8 % 8));
  // The storage still has to work: the rounding must not hand back a slot that
  // overlaps its neighbours or runs off the reservation.
  for (int i = 0; i < 64; i++) { l64[i] = (char)i; g64[i] = (char)(63 - i); }
  printf("%d %d %d %d\n", l64[0], l64[63], g64[0], g64[63]);
  return 0;
}`},

	// va_copy expands to a plain assignment -- modernc defines the builtin as
	// `dst = src` -- and a va_list is a pointer to it, so the assignment copied
	// eight bytes where the state is longer. The destination was left holding the
	// source's address instead of the source's state, and the first va_arg through
	// it read whatever that was: a segfault on amd64, a wrong answer on arm64.
	//
	// A copy must be readable independently of the original, and must not disturb
	// it: the two walk the same arguments from the point of the copy onward.
	{"va-copy", `
#include <stdio.h>
#include <stdarg.h>
static int sum(int n, ...) {
  va_list a, b;
  va_start(a, n);
  va_copy(b, a);                 /* b starts where a is now */
  int x = 0, y = 0;
  for (int i = 0; i < n; i++) x += va_arg(a, int);
  for (int i = 0; i < n; i++) y += va_arg(b, int);   /* the same arguments again */
  va_end(a); va_end(b);
  return x * 100 + y;
}
static int mid(int n, ...) {
  va_list a, b;
  va_start(a, n);
  int first = va_arg(a, int);    /* advance a, THEN copy: b must start here */
  va_copy(b, a);
  int ra = va_arg(a, int), rb = va_arg(b, int);
  va_end(a); va_end(b);
  return first * 100 + ra * 10 + rb;
}
int main(void){
  printf("%d\n", sum(4, 1, 2, 3, 4));   /* 1010: both walks see 10 */
  printf("%d\n", mid(3, 7, 8, 9));      /* 788: b resumes where a was */
  return 0;
}`},

	// --- control-flow stress: the shapes jump threading, short-circuit lowering,
	// and CFG simplification rewrite. Each computes a checksum over many iterations so
	// a wrong edge or dropped value shows up as a mismatch, and each terminates so a
	// miscompiled loop shows up as a timeout.

	{"short-circuit-and-or", `
#include <stdio.h>
int main(void){
  long acc = 0;
  for (long i = 0; i < 200; i++) {
    long a = i & 1, b = i & 2, c = i & 4;
    if (a && b) acc += 1;
    if (a || b) acc += 2;
    if (a && b && c) acc += 4;
    if (a || b || c) acc += 8;
    if ((a && b) || c) acc += 16;
    if (a && (b || c)) acc += 32;
    acc += (a && b) ? 100 : 200;
    acc += (a || c) ? 1000 : 2000;
  }
  printf("%ld\n", acc);
  return 0;
}`},

	{"sentinel-return", `
#include <stdio.h>
#define UNDEF 0x7fffffffL
static long fast(long a, long b) {
  if ((a & 1) && (b & 1)) return a + b;
  if ((a & 2) && (b & 2)) return a * b;
  return UNDEF;                     /* fall back */
}
static long slow(long a, long b) { return a - b; }
int main(void){
  long acc = 0;
  for (long i = 0; i < 300; i++) {
    long r = fast(i, i + 1);
    if (r == UNDEF) r = slow(i, i + 1);   /* branch on the sentinel */
    acc += r;
  }
  printf("%ld\n", acc);
  return 0;
}`},

	{"nested-conditions-loop", `
#include <stdio.h>
int main(void){
  long acc = 0;
  for (long i = 0; i < 150; i++) {
    for (long j = 0; j < 5; j++) {
      if (i > j && (i + j) % 3 == 0) {
        if (i % 2 == 0 || j == 2) acc += i * j;
        else acc -= j;
      } else if (i < j || (i ^ j) & 1) {
        acc += (i > 100) ? j : -j;
      }
    }
  }
  printf("%ld\n", acc);
  return 0;
}`},

	{"switch-and-fallthrough", `
#include <stdio.h>
int main(void){
  long acc = 0;
  for (long i = 0; i < 400; i++) {
    switch (i % 7) {
      case 0: acc += 1; /* fallthrough */
      case 1: acc += 2; break;
      case 2:
      case 3: acc += (i & 1) ? 10 : 20; break;
      case 4: if (i > 200) acc += 100; else acc += 50; break;
      default: acc += (i % 2 == 0 && i % 3 == 0) ? 7 : 3;
    }
  }
  printf("%ld\n", acc);
  return 0;
}`},

	{"early-exit-loops", `
#include <stdio.h>
static int find(const int *a, int n, int x) {
  for (int i = 0; i < n; i++) {
    if (a[i] == x) return i;
    if (a[i] > x && i > 0 && a[i-1] < x) return -i;
  }
  return -100;
}
int main(void){
  int a[16]; for (int i = 0; i < 16; i++) a[i] = i * i - 20;
  long acc = 0;
  for (int x = -30; x < 260; x++) acc += find(a, 16, x);
  printf("%ld\n", acc);
  return 0;
}`},

	{"goto-and-labels", `
#include <stdio.h>
int main(void){
  long acc = 0;
  for (long i = 0; i < 100; i++) {
    long j = 0;
  again:
    if (j >= i) goto done;
    if ((i + j) & 1) { acc += j; j += 2; goto again; }
    acc -= 1; j += 1; goto again;
  done:
    acc += i;
  }
  printf("%ld\n", acc);
  return 0;
}`},

	{"boolean-arith", `
#include <stdio.h>
int main(void){
  long acc = 0;
  for (long i = 0; i < 256; i++) {
    long b0 = (i & 1) != 0, b1 = (i & 2) != 0, b2 = (i & 4) != 0;
    acc += b0 + 2*b1 + 4*b2;
    acc += (b0 && b1) + (b1 || b2) * 3;
    long m = (i > 128) && (i < 200);           /* range test */
    acc += m ? i : -i;
    int t = !(i & 7) ? 5 : (i & 3) ? 6 : 7;    /* nested ternary */
    acc += t;
  }
  printf("%ld\n", acc);
  return 0;
}`},

	{"while-do-mixed", `
#include <stdio.h>
int main(void){
  long acc = 0, i = 1;
  while (i < 10000) {
    long n = i, steps = 0;
    do {
      if (n & 1) n = 3*n + 1; else n = n / 2;   /* collatz */
      steps++;
    } while (n != 1 && steps < 500);
    acc += steps;
    i = i * 3 + 1;
  }
  printf("%ld\n", acc);
  return 0;
}`},

	{"pointer-walk-cond", `
#include <stdio.h>
struct node { long v; int next; };
int main(void){
  struct node pool[32];
  for (int i = 0; i < 32; i++) { pool[i].v = i*7 % 13; pool[i].next = (i+1) % 32; }
  long acc = 0;
  int cur = 0;
  for (int steps = 0; steps < 500; steps++) {
    struct node *p = &pool[cur];
    if (p->v > 5 && p->v < 11) acc += p->v;
    else if (p->v == 0 || p->v == 12) acc -= 1;
    cur = p->next;
    if (cur == 0 && steps > 100) break;
  }
  printf("%ld\n", acc);
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
		// Verify SSA well-formedness after optimization: a pass that leaves a use with
		// no definition, or a phi disagreeing with the CFG, is a bug the differential
		// run might not exercise (it depends on inputs) but that verification catches
		// unconditionally.
		require.NoError(t, ir.VerifyModule(m), "optimized IR is malformed")
		require.NoError(t, analysis.VerifyDominanceModule(m), "optimized IR breaks SSA dominance")
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
	// A bounded run: a miscompile that turns a terminating program into an infinite
	// loop (the exact failure a wrong jump-threading edge produces) must surface as a
	// test failure, not hang the suite until its global deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("run %s: timed out after 10s (likely a miscompiled infinite loop)", bin)
	}
	if err != nil {
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
