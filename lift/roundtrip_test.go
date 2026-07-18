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
		t.Run(c.name, func(t *testing.T) {
			m, err := parse.Parse(c.il)
			require.NoError(t, err)
			params, retty, voidRet := sigOf(m, c.fn)

			o, err := arm64.CompileToObject(m)
			require.NoError(t, err)
			code := extractText(t, o, c.fn)

			// Fresh parse for the lifted comparison, so the original module is not
			// mutated by mem2reg between input vectors.
			lifted, err := lift.Lift(c.fn, code, params, retty, voidRet)
			require.NoError(t, err, "lift")

			for _, args := range c.args {
				want := interpCall(t, mustParse(t, c.il), c.fn, args)
				got := interpCall(t, freshCopy(t, lifted), c.fn, args)
				require.Equalf(t, want, got, "raw-lift args=%v", args)
			}

			// Also validate after mem2reg promotes the register slots to SSA.
			promoted := freshCopy(t, lifted)
			for _, f := range promoted.Funcs {
				opt.Mem2Reg(f)
			}
			require.NoError(t, ir.VerifyModule(promoted))
			for _, args := range c.args {
				want := interpCall(t, mustParse(t, c.il), c.fn, args)
				got := interpCall(t, promoted, c.fn, args)
				require.Equalf(t, want, got, "mem2reg-lift args=%v", args)
			}
		})
	}
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
