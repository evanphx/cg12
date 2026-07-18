package wasm_test

import (
	"testing"

	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// runParsed parses QBE-style IL (whose pointers are all class l / i64), compiles
// it to wasm, and invokes fn — proving parsed pointers work on a 32-bit target
// via address wrapping, with no pointer-inference pass.
func runParsed(t *testing.T, il, fn string, args ...string) string {
	t.Helper()
	m, err := parse.Parse(il)
	require.NoErrorf(t, err, "parse")
	return runFunc(t, m, fn, args...)
}

func TestE2EParsedArrayPointers(t *testing.T) {
	// buf[i] = i*i for i in [0,n); then sum. All pointers are class l (i64):
	// alloc, extsw+mul+add indexing, storew/loadw through wrapped addresses.
	const il = `export function w $sqsum(w %n) {
@start
	%buf =l alloc4 400
	jmp @fill
@fill
	%i =w phi @start 0, @fbody %i2
	%cf =w csgew %i, %n
	jnz %cf, @sum, @fbody
@fbody
	%il =l extsw %i
	%off =l mul %il, 4
	%addr =l add %buf, %off
	%sq =w mul %i, %i
	storew %sq, %addr
	%i2 =w add %i, 1
	jmp @fill
@sum
	%j =w phi @fill 0, @sbody %j2
	%acc =w phi @fill 0, @sbody %acc2
	%cs =w csgew %j, %n
	jnz %cs, @exit, @sbody
@sbody
	%jl =l extsw %j
	%soff =l mul %jl, 4
	%saddr =l add %buf, %soff
	%v =w loadw %saddr
	%acc2 =w add %acc, %v
	%j2 =w add %j, 1
	jmp @sum
@exit
	ret %acc
}`
	require.Equal(t, "30", runParsed(t, il, "sqsum", "5")) // 0+1+4+9+16
	require.Equal(t, "0", runParsed(t, il, "sqsum", "0"))
}

func TestE2EParsedNestedLoops(t *testing.T) {
	// Two nested loops. Parsed blocks share Block.ID 0, so loop labels must be
	// made unique from the reverse-postorder index — otherwise the inner and
	// outer loops collide and a back-edge branches to the wrong one.
	const il = `export function w $nest(w %n) {
@start
	jmp @outer
@outer
	%i =w phi @start 0, @ocont %i2
	%acc =w phi @start 0, @ocont %acc2
	%co =w csgew %i, %n
	jnz %co, @exit, @oin
@oin
	jmp @inner
@inner
	%j =w phi @oin 0, @iin %j2
	%iacc =w phi @oin %acc, @iin %iacc2
	%ci =w csgew %j, %n
	jnz %ci, @ocont, @iin
@iin
	%iacc2 =w add %iacc, 1
	%j2 =w add %j, 1
	jmp @inner
@ocont
	%acc2 =w copy %iacc
	%i2 =w add %i, 1
	jmp @outer
@exit
	ret %acc
}`
	require.Equal(t, "25", runParsed(t, il, "nest", "5")) // n*n
	require.Equal(t, "0", runParsed(t, il, "nest", "0"))
}

func TestE2EParsedPointerInMemory(t *testing.T) {
	// A pointer is stored to memory (storel, 8 bytes) and loaded back (loadl),
	// then used as an address — the strcmp/scc idiom. The 8-byte pointer image
	// round-trips and the wrapped low 32 bits address linear memory correctly.
	const il = `export function w $indirect(w %x) {
@start
	%slot =l alloc8 8
	%buf =l alloc8 16
	storel %buf, %slot
	%p =l loadl %slot
	storew %x, %p
	%v =w loadw %buf
	ret %v
}`
	require.Equal(t, "99", runParsed(t, il, "indirect", "99"))
	require.Equal(t, "-7", runParsed(t, il, "indirect", "-7"))
}
