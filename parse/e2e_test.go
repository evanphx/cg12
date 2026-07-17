package parse_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assembler(t testing.TB) string {
	t.Helper()
	if p, ok := testenv.Have("aarch64-linux-gnu-gcc"); ok {
		return p
	}
	if runtime.GOARCH != "arm64" {
		t.Skip("no AArch64 toolchain: this host is not arm64 and there is no cross compiler")
	}
	return testenv.Tool(t, "cc", "gcc", "clang")
}

// parseCompileRun parses IL text, optimises it, compiles it, links it with a C
// driver, and returns the program's exit code.
func parseCompileRun(t *testing.T, il, cmain string) int {
	t.Helper()
	cc := assembler(t)
	m, err := parse.Parse(il)
	require.NoErrorf(t, err, "parse:\n%s", il)
	opt.OptimizeModule(m)
	o, err := arm64.CompileToObject(m)
	require.NoError(t, err)
	data, err := o.MarshalELF()
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "o.o"), data, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "m.c"), []byte(cmain), 0o644))
	bin := filepath.Join(dir, "p")
	out, err := exec.Command(cc, "-o", bin, filepath.Join(dir, "o.o"), filepath.Join(dir, "m.c")).CombinedOutput()
	require.NoErrorf(t, err, "link failed: %s\n%s", out, arm64.Disassemble(o))

	cmd := exec.Command(bin)
	var sb bytes.Buffer
	cmd.Stdout = &sb
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	require.NoError(t, err)
	return 0
}

func TestParseCompileArith(t *testing.T) {
	il := `
export function w $addmul(w %a, w %b) {
@start
	%t =w add %a, %b
	%r =w mul %t, %a
	ret %r
}
`
	code := parseCompileRun(t, il, `
extern int addmul(int,int);
int main(void){ return addmul(3,4)==21 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestParseCompileLoop(t *testing.T) {
	il := `
export function w $sumto(w %n) {
@start
	jmp @loop
@loop
	%i =w phi @start 0, @body %i2
	%s =w phi @start 0, @body %s2
	%c =w csgew %i, %n
	jnz %c, @exit, @body
@body
	%s2 =w add %s, %i
	%i2 =w add %i, 1
	jmp @loop
@exit
	ret %s
}
`
	code := parseCompileRun(t, il, `
extern int sumto(int);
int main(void){ return sumto(10)==45 && sumto(100)==4950 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestParseCompileAggregateGP(t *testing.T) {
	// A small struct { w, w } is passed by value in two halves of one x
	// register bank; sumpair reconstructs it and reads both fields. mk builds a
	// pair on the stack and passes it by value.
	il := `
type :pair = { w, w }

export function w $sumpair(:pair %p) {
@start
	%a =w loaduw %p
	%p4 =l add %p, 4
	%b =w loaduw %p4
	%r =w add %a, %b
	ret %r
}

export function w $mk(w %x, w %y) {
@start
	%s =l alloc8 8
	storew %x, %s
	%s4 =l add %s, 4
	storew %y, %s4
	%r =w call $sumpair(:pair %s)
	ret %r
}
`
	code := parseCompileRun(t, il, `
typedef struct { int a, b; } pair;
extern int sumpair(pair);
extern int mk(int,int);
int main(void){
  pair p = { 3, 4 };
  if (sumpair(p) != 7) return 1;
  if (mk(10, 20) != 30) return 2;   // cg12 passing a struct to cg12
  return 0;
}`)
	assert.Equal(t, 0, code)
}

func TestParseCompileAggregateHFA(t *testing.T) {
	// { s, s } is a homogeneous float aggregate, passed in two SIMD registers.
	il := `
type :vec2 = { s, s }

export function s $vsum(:vec2 %v) {
@start
	%x =s loads %v
	%v4 =l add %v, 4
	%y =s loads %v4
	%r =s add %x, %y
	ret %r
}
`
	code := parseCompileRun(t, il, `
typedef struct { float x, y; } vec2;
extern float vsum(vec2);
int main(void){ vec2 v = { 1.5f, 2.5f }; return vsum(v) == 4.0f ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestParseCompileAggregateReturnGP(t *testing.T) {
	// { w, w } is returned in a single x register; C reads it back by value.
	il := `
type :pair = { w, w }

export function :pair $mkpair(w %a, w %b) {
@start
	%p =l alloc8 8
	storew %a, %p
	%p4 =l add %p, 4
	storew %b, %p4
	ret %p
}
`
	code := parseCompileRun(t, il, `
typedef struct { int a, b; } pair;
extern pair mkpair(int, int);
int main(void){ pair p = mkpair(3, 4); return (p.a == 3 && p.b == 4) ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestParseCompileAggregateReturnHFA(t *testing.T) {
	// { s, s } is a homogeneous float aggregate, returned in s0/s1.
	il := `
type :vec2 = { s, s }

export function :vec2 $mkvec(s %x, s %y) {
@start
	%p =l alloc8 8
	stores %x, %p
	%p4 =l add %p, 4
	stores %y, %p4
	ret %p
}
`
	code := parseCompileRun(t, il, `
typedef struct { float x, y; } vec2;
extern vec2 mkvec(float, float);
int main(void){ vec2 v = mkvec(1.5f, 2.5f); return (v.x == 1.5f && v.y == 2.5f) ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestParseCompileAggregateReturnMemoryRoundTrip(t *testing.T) {
	// mkbig returns a 24-byte struct via the x8 indirect-result register; sumbig
	// is a cg12 caller that allocates the buffer, receives the result, and reads
	// it — exercising both sides of a Memory-class return.
	il := `
type :big = { l, l, l }

export function :big $mkbig(l %v) {
@start
	%p =l alloc8 24
	storel %v, %p
	%p8 =l add %p, 8
	storel %v, %p8
	%p16 =l add %p, 16
	storel %v, %p16
	ret %p
}

export function l $sumbig(l %v) {
@start
	%b =:big call $mkbig(l %v)
	%x0 =l loadl %b
	%b8 =l add %b, 8
	%x1 =l loadl %b8
	%b16 =l add %b, 16
	%x2 =l loadl %b16
	%s =l add %x0, %x1
	%r =l add %s, %x2
	ret %r
}
`
	code := parseCompileRun(t, il, `
extern long sumbig(long);
int main(void){ return sumbig(7) == 21 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestParseCompileStackedAggregate(t *testing.T) {
	// Eight ints fill x0..x7, so the struct is passed on the stack; the callee
	// reads it as the address of the incoming bytes.
	il := `
type :pair = { w, w }

export function w $stk(w %a, w %b, w %c, w %d, w %e, w %f, w %g, w %h, :pair %p) {
@start
	%x =w loaduw %p
	%p4 =l add %p, 4
	%y =w loaduw %p4
	%s =w add %x, %y
	%r =w add %s, %a
	ret %r
}
`
	code := parseCompileRun(t, il, `
typedef struct { int a, b; } pair;
extern int stk(int,int,int,int,int,int,int,int,pair);
int main(void){ pair p = { 10, 20 }; return stk(1,2,3,4,5,6,7,8,p) == 31 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestParseCompileVariadic(t *testing.T) {
	// A variadic function summing n trailing int arguments via vaarg, mixing the
	// register save area and (for n > 8) the overflow stack.
	il := `
export function w $vsum(w %n, ...) {
@start
	%ap =l alloc8 32
	vastart %ap
	jmp @loop
@loop
	%i =w phi @start 0, @body %i2
	%acc =w phi @start 0, @body %acc2
	%done =w csgew %i, %n
	jnz %done, @exit, @body
@body
	%v =w vaarg %ap
	%acc2 =w add %acc, %v
	%i2 =w add %i, 1
	jmp @loop
@exit
	ret %acc
}
`
	code := parseCompileRun(t, il, `
extern int vsum(int, ...);
int main(void){
  // 10 args forces two onto the overflow stack.
  if (vsum(3, 10, 20, 30) != 60) return 1;
  if (vsum(10, 1,2,3,4,5,6,7,8,9,10) != 55) return 2;
  return 0;
}`)
	assert.Equal(t, 0, code)
}

func TestParseCompileVariadicFloat(t *testing.T) {
	// Variadic doubles: register save area then overflow stack.
	il := `
export function d $dvsum(w %n, ...) {
@start
	%ap =l alloc8 32
	vastart %ap
	jmp @loop
@loop
	%i =w phi @start 0, @body %i2
	%acc =d phi @start d_0, @body %acc2
	%done =w csgew %i, %n
	jnz %done, @exit, @body
@body
	%v =d vaarg %ap
	%acc2 =d add %acc, %v
	%i2 =w add %i, 1
	jmp @loop
@exit
	ret %acc
}
`
	code := parseCompileRun(t, il, `
extern double dvsum(int, ...);
int main(void){
  double r = dvsum(10, 1.0,2.0,3.0,4.0,5.0,6.0,7.0,8.0,9.0,10.0); // 55
  return r == 55.0 ? 0 : 1;
}`)
	assert.Equal(t, 0, code)
}

func TestParseCompileLargeFrame(t *testing.T) {
	// A large stack allocation forces a frame bigger than the stp pre-index
	// immediate can reach, so the prologue subtracts sp separately.
	il := `
export function w $bigframe(w %x) {
@start
	%p =l alloc4 2048
	storew %x, %p
	%p100 =l add %p, 100
	%x2 =w mul %x, 2
	storew %x2, %p100
	%a =w loaduw %p
	%b =w loaduw %p100
	%r =w add %a, %b
	ret %r
}
`
	code := parseCompileRun(t, il, `
extern int bigframe(int);
int main(void){ return bigframe(14) == 14 + 28 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestParseCompileFloatBits(t *testing.T) {
	// QBE allows a floating operand written as a raw integer bit pattern.
	il := `
export function w $le1(d %x) {
@start
	%c =w cled %x, 4607182418800017408
	ret %c
}
`
	code := parseCompileRun(t, il, `
extern int le1(double);
int main(void){ return (le1(0.5) == 1 && le1(2.0) == 0) ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestParseCompileMemoryAndFloat(t *testing.T) {
	// A stack slot (promoted by mem2reg after parsing) and float arithmetic.
	il := `
export function d $scaled(w %n, d %k) {
@start
	%p =l alloc8 8
	%nd =l extsw %n
	%f =d sltof %nd
	%r =d mul %f, %k
	stored %r, %p
	%out =d loadd %p
	ret %out
}
`
	code := parseCompileRun(t, il, `
extern double scaled(int, double);
int main(void){ return scaled(4, 2.5) == 10.0 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}
