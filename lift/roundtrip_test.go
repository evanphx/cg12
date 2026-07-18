package lift_test

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/lift"
	"github.com/evanphx/cg12/obj"
	"github.com/evanphx/cg12/opt"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// The round trip: compile a function to arm64, lift the machine code back to IR,
// and require the interpreter to agree on the original and the lifted IR over
// many inputs. The interpreter is the oracle; a divergence localizes a lifter (or
// backend) bug. No assembler, no qemu.
func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		fn   string
		il   string
		args [][]interp.Value
	}{
		{
			name: "add", fn: "add",
			il:   `export function l $add(l %a, l %b) { @s %r =l add %a, %b  ret %r }`,
			args: pairs(3, 4, -1, 100, 0, 0),
		},
		{
			name: "arith", fn: "f",
			il: `export function l $f(l %a, l %b) {
				@s
				%x =l mul %a, %b
				%y =l sub %x, %a
				%z =l add %y, 7
				ret %z
			}`,
			args: pairs(3, 4, 5, 6, -2, 9),
		},
		{
			name: "word", fn: "f",
			il: `export function w $f(w %a, w %b) {
				@s
				%x =w add %a, %b
				%y =w mul %x, 3
				ret %y
			}`,
			args: pairs(3, 4, 1000000, 2000000, -5, 5),
		},
		{
			name: "max-branch", fn: "max",
			il: `export function l $max(l %a, l %b) {
				@s
				%c =w csgtl %a, %b
				jnz %c, @ta, @tb
				@ta
				ret %a
				@tb
				ret %b
			}`,
			args: pairs(3, 4, 9, 2, -1, -5),
		},
		{
			name: "sum-loop", fn: "sum",
			il: `export function l $sum(l %n) {
				@start
				jmp @loop
				@loop
				%i =l phi @start 1, @loop %i1
				%s =l phi @start 0, @loop %s1
				%s1 =l add %s, %i
				%i1 =l add %i, 1
				%c =w csgtl %i1, %n
				jnz %c, @exit, @loop
				@exit
				ret %s1
			}`,
			args: singles(10, 100, 1, 0),
		},
		{
			name: "gcd", fn: "gcd",
			il: `export function l $gcd(l %a0, l %b0) {
				@start
				jmp @loop
				@loop
				%a =l phi @start %a0, @loop %b
				%b =l phi @start %b0, @loop %r
				%r =l rem %a, %b
				%done =w ceql %r, 0
				jnz %done, @exit, @loop
				@exit
				ret %b
			}`,
			args: pairs(48, 36, 100, 35, 17, 5),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { roundTrip(t, c.il, c.fn, c.args) })
	}
}

// TestRoundTripFloat closes the same loop for floating-point functions: v-register
// modeling, float arithmetic/compare, and the AAPCS float argument/result
// registers.
func TestRoundTripFloat(t *testing.T) {
	cases := []struct {
		name string
		fn   string
		il   string
		args [][]interp.Value
	}{
		{
			name: "dmath", fn: "f",
			il: `export function d $f(d %x, d %y) {
				@s
				%a =d add %x, %y
				%b =d mul %a, %x
				%c =d sub %b, %y
				ret %c
			}`,
			args: dargs([2]float64{1.5, 2.5}, [2]float64{-3.0, 4.25}, [2]float64{0.1, 0.2}),
		},
		{
			name: "fmax-branch", fn: "fmax",
			il: `export function d $fmax(d %a, d %b) {
				@s
				%c =w cgtd %a, %b
				jnz %c, @ta, @tb
				@ta
				ret %a
				@tb
				ret %b
			}`,
			args: dargs([2]float64{1.5, 2.5}, [2]float64{9.0, 2.0}, [2]float64{-1.0, -5.0}),
		},
		{
			name: "int-to-double", fn: "f",
			il: `export function d $f(l %n) {
				@s
				%d =d sltof %n
				%r =d mul %d, %d
				ret %r
			}`,
			args: [][]interp.Value{{interp.L(7)}, {interp.L(-3)}, {interp.L(100)}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { roundTrip(t, c.il, c.fn, c.args) })
	}
}

// roundTrip compiles the function to arm64, lifts it, and requires the
// interpreter to agree with the original over every input vector — both on the
// raw register-slot form and after mem2reg promotes it to SSA.
func roundTrip(t *testing.T, il, fn string, argVectors [][]interp.Value) {
	t.Helper()
	m, err := parse.Parse(il)
	require.NoError(t, err)
	params, retty, voidRet := sigOf(m, fn)

	o, err := arm64.CompileToObject(m)
	require.NoError(t, err)
	code := extractText(t, o, fn)

	lifted, err := lift.Lift(fn, code, params, retty, voidRet)
	require.NoError(t, err, "lift")

	for _, args := range argVectors {
		want := interpCall(t, mustParse(t, il), fn, args)
		got := interpCall(t, freshCopy(t, lifted), fn, args)
		require.Equalf(t, want, got, "raw-lift args=%v", args)
	}

	promoted := freshCopy(t, lifted)
	for _, f := range promoted.Funcs {
		opt.Mem2Reg(f)
	}
	require.NoError(t, ir.VerifyModule(promoted))
	for _, args := range argVectors {
		want := interpCall(t, mustParse(t, il), fn, args)
		got := interpCall(t, promoted, fn, args)
		require.Equalf(t, want, got, "mem2reg-lift args=%v", args)
	}
}

// dargs builds double argument vectors from pairs.
func dargs(ps ...[2]float64) [][]interp.Value {
	var out [][]interp.Value
	for _, p := range ps {
		out = append(out, []interp.Value{interp.D(p[0]), interp.D(p[1])})
	}
	return out
}

// TestRoundTripCalls lifts whole modules through LiftModule so bl calls resolve:
// a recursive fib that calls itself, and a caller/callee pair.
func TestRoundTripCalls(t *testing.T) {
	cases := []struct {
		name string
		fn   string
		il   string
		args []int64
	}{
		{
			name: "fib", fn: "fib",
			il: `export function l $fib(l %n) {
				@s
				%c =w cslel %n, 1
				jnz %c, @base, @rec
				@base
				ret %n
				@rec
				%n1 =l sub %n, 1
				%a =l call $fib(l %n1)
				%n2 =l sub %n, 2
				%b =l call $fib(l %n2)
				%r =l add %a, %b
				ret %r
			}`,
			args: []int64{0, 1, 5, 10, 15},
		},
		{
			name: "caller-callee", fn: "caller",
			il: `function l $dbl(l %x) { @s %r =l mul %x, 2  ret %r }
				export function l $caller(l %a, l %b) {
				@s
				%x =l call $dbl(l %a)
				%y =l call $dbl(l %b)
				%r =l add %x, %y
				ret %r
			}`,
			args: []int64{3, 4, 100, -5},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := parse.Parse(c.il)
			require.NoError(t, err)
			o, err := arm64.CompileToObject(m)
			require.NoError(t, err)
			lifted, err := lift.LiftModule(m, o)
			require.NoError(t, err, "lift module")

			call := func(mod *ir.Module, args []interp.Value) uint64 {
				return interpCall(t, mod, c.fn, args)
			}
			// caller takes two args, fib one; build the vector from the flat list.
			vectors := singleOrPair(c.fn, c.args)

			promoted := freshCopy(t, lifted)
			for _, f := range promoted.Funcs {
				opt.Mem2Reg(f)
			}
			require.NoError(t, ir.VerifyModule(promoted))

			for _, args := range vectors {
				want := call(mustParse(t, c.il), args)
				require.Equal(t, want, call(freshCopy(t, lifted), args), "raw args=%v", args)
				require.Equal(t, want, call(promoted, args), "mem2reg args=%v", args)
			}
		})
	}
}

// singleOrPair packs the flat argument list as one-arg vectors for fib, two-arg
// for caller.
func singleOrPair(fn string, v []int64) [][]interp.Value {
	if fn == "caller" {
		return pairs(v...)
	}
	return singles(v...)
}

func sigOf(m *ir.Module, name string) ([]ir.Cls, ir.Cls, bool) {
	for _, f := range m.Funcs {
		if f.Name == name {
			var ps []ir.Cls
			for _, p := range f.Params {
				ps = append(ps, p.Cls)
			}
			return ps, f.Retty, !f.HasRet
		}
	}
	return nil, 0, false
}

func extractText(t *testing.T, o *obj.Object, name string) []uint32 {
	t.Helper()
	for _, s := range o.Syms {
		if s.Name == name && s.Section == obj.SecText {
			code := o.Text[s.Value : s.Value+s.Size]
			words := make([]uint32, len(code)/4)
			for i := range words {
				words[i] = uint32(code[i*4]) | uint32(code[i*4+1])<<8 | uint32(code[i*4+2])<<16 | uint32(code[i*4+3])<<24
			}
			return words
		}
	}
	t.Fatalf("no .text symbol %q", name)
	return nil
}

func interpCall(t *testing.T, m *ir.Module, name string, args []interp.Value) uint64 {
	t.Helper()
	mc, err := interp.New(m)
	require.NoError(t, err)
	v, err := mc.Call(name, args...)
	require.NoError(t, err)
	return v.U64()
}

func mustParse(t *testing.T, il string) *ir.Module {
	t.Helper()
	m, err := parse.Parse(il)
	require.NoError(t, err)
	return m
}

// freshCopy round-trips a module through its binary form so each interpreter run
// (and mem2reg) works on an independent copy.
func freshCopy(t *testing.T, m *ir.Module) *ir.Module {
	t.Helper()
	data, err := m.MarshalBinary()
	require.NoError(t, err)
	c, err := ir.DecodeModule(data)
	require.NoError(t, err)
	return c
}

// pairs builds argument vectors of two longs from a flat list.
func pairs(v ...int64) [][]interp.Value {
	var out [][]interp.Value
	for i := 0; i+1 < len(v); i += 2 {
		out = append(out, []interp.Value{interp.L(v[i]), interp.L(v[i+1])})
	}
	return out
}

// singles builds one-argument vectors of a single long each.
func singles(v ...int64) [][]interp.Value {
	var out [][]interp.Value
	for _, x := range v {
		out = append(out, []interp.Value{interp.L(x)})
	}
	return out
}
