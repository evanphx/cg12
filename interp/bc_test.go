package interp_test

import (
	"bytes"
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
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
		{
			name: "aggregate-return-param-arg",
			il: `type :pt = { w, w }
				function :pt $mkpt(w %x, w %y) {
				@start
				%r =l alloc4 8
				storew %x, %r
				%r4 =l add %r, 4
				storew %y, %r4
				ret %r
				}
				function w $sumpt(:pt %p) {
				@start
				%a =w loadw %p
				%p4 =l add %p, 4
				%b =w loadw %p4
				%s =w add %a, %b
				ret %s
				}
				export function w $test() {
				@start
				%p =:pt call $mkpt(w 3, w 4)
				%s =w call $sumpt(:pt %p)
				ret %s
				}`,
			fn: "test",
		},
		{
			name: "aggregate-arg-copied",
			il: `type :pt = { w, w }
				function $clobber(:pt %p) {
				@start
				storew 99, %p
				ret
				}
				export function w $test() {
				@start
				%p =l alloc4 8
				storew 5, %p
				call $clobber(:pt %p)
				%v =w loadw %p
				ret %v
				}`,
			fn: "test",
		},
		{
			name: "select",
			il:   `export function l $f(l %c, l %a, l %b) { @s %r =l select %c, %a, %b  ret %r }`,
			fn:   "f", args: []interp.Value{interp.L(1), interp.L(10), interp.L(20)},
		},
		{
			name: "select-zero",
			il:   `export function l $f(l %c, l %a, l %b) { @s %r =l select %c, %a, %b  ret %r }`,
			fn:   "f", args: []interp.Value{interp.L(0), interp.L(10), interp.L(20)},
		},
		{
			name: "stack-intrinsics",
			// stacksave/stackrestore bracket a VLA; both engines run the same
			// intrinsic dispatch and must agree on the value read from it.
			il: `export function w $f() {
				@s
				%sp =l intrinsic stacksave
				%p =l alloc4 16
				storew 7, %p
				%v =w loadw %p
				intrinsic stackrestore %sp
				ret %v
			}`,
			fn: "f",
		},
		{
			name: "allocn",
			// A runtime-sized (VLA) stack allocation: n*16 bytes, written and read
			// back, exercised under both engines.
			il: `export function w $f(l %n) {
				@s
				%sz =l mul %n, 16
				%p =l allocn %sz
				storew 7, %p
				%v =w loadw %p
				ret %v
			}`,
			fn: "f", args: []interp.Value{interp.L(2)},
		},
		{
			name: "variadic-sum",
			il: `export function w $sum(w %n, ...) {
				@start
				%ap =l alloc8 8
				vastart %ap
				jmp @loop
				@loop
				%i =w phi @start 0, @body %i1
				%t =w phi @start 0, @body %t1
				%done =w csgew %i, %n
				jnz %done, @exit, @body
				@body
				%v =w vaarg %ap
				%t1 =w add %t, %v
				%i1 =w add %i, 1
				jmp @loop
				@exit
				ret %t }`,
			fn: "sum", args: []interp.Value{interp.W(3), interp.W(10), interp.W(20), interp.W(30)},
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

// printf (a variadic extern) produces identical output under both engines.
func TestBytecodePrintfMatches(t *testing.T) {
	il := `data $fmt = { b "n=%d s=%s x=%x", b 0 }
	data $str = { b "hi", b 0 }
	export function w $go() {
	@start
		%r =w call $printf(l $fmt, w 42, l $str, w 255, ...)
		ret %r }`
	m, err := parse.Parse(il)
	require.NoError(t, err)

	var twBuf, bcBuf bytes.Buffer
	tw, err := interp.New(m, interp.WithStdout(&twBuf))
	require.NoError(t, err)
	_, err = tw.Call("go")
	require.NoError(t, err)

	vm, err := interp.New(m, interp.WithStdout(&bcBuf), interp.WithBytecode())
	require.NoError(t, err)
	_, err = vm.Call("go")
	require.NoError(t, err)

	require.Equal(t, twBuf.String(), bcBuf.String())
	require.Equal(t, "n=42 s=hi x=ff", bcBuf.String())
}

// A computed goto (blockaddr + br) runs the same under both engines. The parser
// never emits these ops, so the function is built directly.
func TestBytecodeComputedGoto(t *testing.T) {
	build := func() *ir.Module {
		m := &ir.Module{}
		f := m.NewFunc("pick", ir.ClsW).Export()
		sel := f.Param("sel", ir.ClsW)
		entry := f.Entry()
		tA := f.NewBlock("tA")
		tB := f.NewBlock("tB")
		disp := f.NewBlock("disp")
		blkA := f.NewBlock("A")
		blkB := f.NewBlock("B")
		aA := entry.BlockAddr(blkA)
		aB := entry.BlockAddr(blkB)
		entry.Jnz(sel, tA, tB)
		tA.Goto(disp)
		tB.Goto(disp)
		addr := disp.Phi(ir.ClsL, ir.PhiEdge{From: tA, Val: aA}, ir.PhiEdge{From: tB, Val: aB})
		disp.BrIndirect(addr, blkA, blkB)
		blkA.Ret(f.Word(10))
		blkB.Ret(f.Word(20))
		return m
	}
	for _, sel := range []int32{1, 0} {
		tw, err := interp.New(build())
		require.NoError(t, err)
		got, err := tw.Call("pick", interp.W(sel))
		require.NoError(t, err)

		vm, err := interp.New(build(), interp.WithBytecode())
		require.NoError(t, err)
		gotVM, err := vm.Call("pick", interp.W(sel))
		require.NoError(t, err)
		require.Equal(t, got, gotVM, "sel=%d", sel)
	}
}
