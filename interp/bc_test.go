package interp_test

import (
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// runBC runs one exported function through the bytecode VM.
func runBC(t *testing.T, il, fn string, args ...interp.Value) interp.Value {
	t.Helper()
	m, err := parse.Parse(il)
	require.NoError(t, err, "parse")
	mc, err := interp.New(m, interp.WithBytecode())
	require.NoError(t, err, "load")
	v, err := mc.Call(fn, args...)
	require.NoError(t, err, "call")
	return v
}

// The bytecode VM must agree with the tree-walker on every program: same IR, two
// independent execution strategies.
func TestBytecodeMatchesTreeWalker(t *testing.T) {
	cases := []struct {
		name string
		il   string
		fn   string
		args []interp.Value
	}{
		{
			name: "add",
			il:   `export function w $f(w %a, w %b) { @s %r =w add %a, %b  ret %r }`,
			fn:   "f", args: []interp.Value{interp.W(17), interp.W(25)},
		},
		{
			name: "arith-consts",
			il:   `export function w $f() { @s %a =w add 20, 22  %b =w mul %a, 5  %c =w sub %b, 3  ret %c }`,
			fn:   "f",
		},
		{
			name: "loop-sum",
			il: `export function l $f(l %n) {
				@start
				jmp @loop
				@loop
				%i =l phi @start 0, @loop %i1
				%s =l phi @start 0, @loop %s1
				%s1 =l add %s, %i
				%i1 =l add %i, 1
				%done =w csgel %i1, %n
				jnz %done, @exit, @loop
				@exit
				ret %s1
			}`,
			fn: "f", args: []interp.Value{interp.L(10)},
		},
		{
			name: "swap-cycle",
			il: `export function w $f(w %n) {
				@start
				jmp @loop
				@loop
				%i =w phi @start 0, @loop %i1
				%a =w phi @start 1, @loop %b
				%b =w phi @start 2, @loop %a
				%i1 =w add %i, 1
				%done =w csgew %i1, %n
				jnz %done, @exit, @loop
				@exit
				%r =w mul %a, 10
				%r2 =w add %r, %b
				ret %r2
			}`,
			fn: "f", args: []interp.Value{interp.W(3)},
		},
		{
			name: "recursion-fact",
			il: `export function l $fact(l %n) {
				@start
				%z =w cslel %n, 1
				jnz %z, @base, @rec
				@base
				ret 1
				@rec
				%n1 =l sub %n, 1
				%r =l call $fact(l %n1)
				%p =l mul %n, %r
				ret %p
			}`,
			fn: "fact", args: []interp.Value{interp.L(6)},
		},
		{
			name: "memory",
			il: `export function w $f() {
				@s
				%p =l alloc4 16
				storew 42, %p
				%p4 =l add %p, 4
				storew 100, %p4
				%a =w loadw %p
				%b =w loadw %p4
				%r =w add %a, %b
				ret %r
			}`,
			fn: "f",
		},
		{
			name: "float",
			il: `export function d $f(d %x, d %y) {
				@s
				%a =d add %x, %y
				%b =d mul %a, %a
				ret %b
			}`,
			fn: "f", args: []interp.Value{interp.D(1.5), interp.D(2.5)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tw := run(t, c.il, c.fn, c.args...)
			bc := runBC(t, c.il, c.fn, c.args...)
			require.Equal(t, tw, bc, "tree-walker vs bytecode")
		})
	}
}
