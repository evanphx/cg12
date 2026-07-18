package difftest

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// The QBE aggregate-ABI corpus (abi1-6) exercises by-value struct arguments,
// parameters, and returns. The interpreter is target-independent — it passes
// aggregates by value through memory and does not model register-vs-stack
// placement — so each file's C driver is transcribed here as Go: internal IL
// functions run in the interpreter, driver-defined functions are registered as
// externs, and the result is compared to the embedded `# >>> output`.
func TestInterpABI(t *testing.T) {
	t.Run("abi1", testABI1)
	t.Run("abi2", testABI2)
	t.Run("abi3", testABI3)
	t.Run("abi4", testABI4)
	t.Run("abi5", testABI5)
	t.Run("abi6", testABI6)
}

func loadABI(t *testing.T, file string) (*ir.Module, qbeTest) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "qbe", file))
	require.NoError(t, err)
	m, err := parse.Parse(string(src))
	require.NoError(t, err, "parse %s", file)
	return m, loadQBETest(string(src))
}

func storeF32(t *testing.T, mc *interp.Machine, addr uint64, v float32) {
	t.Helper()
	require.NoError(t, mc.Store(addr, 4, uint64(math.Float32bits(v))))
}

func storeF64(t *testing.T, mc *interp.Machine, addr uint64, v float64) {
	t.Helper()
	require.NoError(t, mc.Store(addr, 8, math.Float64bits(v)))
}

func storeI64(mc *interp.Machine, addr uint64, v int64) {
	mc.Store(addr, 8, uint64(v))
}

func loadF32(mc *interp.Machine, addr uint64) float32 {
	b, _ := mc.ReadBytes(addr, 4)
	return math.Float32frombits(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24)
}

func readCStr(mc *interp.Machine, addr uint64) string {
	var out []byte
	for {
		b, err := mc.ReadBytes(addr, 1)
		if err != nil || b[0] == 0 {
			return string(out)
		}
		out = append(out, b[0])
		addr++
	}
}

// abi2: internal sum(:fps) reads two floats of a by-value struct. The driver
// builds fps{1.23, -1, 2.34} and checks sum == 1.23f + 2.34f.
func testABI2(t *testing.T) {
	m, _ := loadABI(t, "abi2.ssa")
	mc, err := interp.New(m)
	require.NoError(t, err)

	// fps = { s @0, b @4, s @8 } — 12 bytes.
	p := mc.Alloc(12)
	storeF32(t, mc, p, 1.23)
	require.NoError(t, mc.Store(p+4, 1, 0xff)) // char -1, unread by sum
	storeF32(t, mc, p+8, 2.34)

	v, err := mc.Call("sum", interp.Ptr(p))
	require.NoError(t, err)
	require.Equal(t, float32(1.23)+float32(2.34), v.F32())
}

// abi4: internal test() returns a large struct (:mem, 17 bytes) that alpha fills
// with 'A'..'P'. The driver prints it with "%s\n".
func testABI4(t *testing.T) {
	m, tc := loadABI(t, "abi4.ssa")
	mc, err := interp.New(m)
	require.NoError(t, err)

	r, err := mc.Call("test")
	require.NoError(t, err)
	got := readCStr(mc, r.U64()) + "\n"
	require.Equal(t, tc.output, got)
}

// abi6: internal f() receives three HFA structs by value and two doubles, calling
// the driver's phfa3 on each struct and printf on each double.
func testABI6(t *testing.T) {
	m, tc := loadABI(t, "abi6.ssa")

	var buf bytes.Buffer
	phfa3 := func(mc *interp.Machine, args []interp.Value) (interp.Value, error) {
		h := args[0].U64() // pointer to hfa3 { s, s, s }
		fmt.Fprintf(&buf, "{ %g, %g, %g }\n", loadF32(mc, h), loadF32(mc, h+4), loadF32(mc, h+8))
		return interp.Value{}, nil
	}
	mc, err := interp.New(m, interp.WithStdout(&buf), interp.WithExtern("phfa3", phfa3))
	require.NoError(t, err)

	mkHfa := func(a, b, c float32) uint64 {
		p := mc.Alloc(12)
		storeF32(t, mc, p, a)
		storeF32(t, mc, p+4, b)
		storeF32(t, mc, p+8, c)
		return p
	}
	h1, h2, h3 := mkHfa(1, 2, 3), mkHfa(2, 3, 4), mkHfa(3, 4, 5)
	_, err = mc.Call("f", interp.Ptr(h1), interp.Ptr(h2), interp.D(1), interp.Ptr(h3), interp.D(2))
	require.NoError(t, err)
	require.Equal(t, tc.output, buf.String())
}

// abi3: internal test() calls the driver's F through a computed function pointer,
// passing a :four struct by value, then stores F's result into the global `a`.
func testABI3(t *testing.T) {
	m, tc := loadABI(t, "abi3.ssa")

	var buf bytes.Buffer
	fFn := func(mc *interp.Machine, args []interp.Value) (interp.Value, error) {
		s := args[4].U64() // pointer to :four { l @0, b @8, w @12 }
		sl, _ := mc.ReadBytes(s, 8)
		var l int64
		for i, by := range sl {
			l |= int64(by) << (8 * i)
		}
		si, _ := mc.ReadBytes(s+12, 4)
		var i int32
		for k, by := range si {
			i |= int32(by) << (8 * k)
		}
		fmt.Fprintf(&buf, "%d %d %d %d %d %d %d\n",
			args[0].I64(), args[1].I64(), args[2].I64(), args[3].I64(), l, i, args[5].I64())
		return interp.W(42), nil
	}
	mc, err := interp.New(m, interp.WithExtern("F", fFn))
	require.NoError(t, err)
	mc.DefineExtern("a", 4, 4) // int a; the driver's global

	_, err = mc.Call("test")
	require.NoError(t, err)

	// main() then prints the global a.
	aAddr, _ := mc.GlobalAddr("a")
	ab, _ := mc.ReadBytes(aAddr, 4)
	fmt.Fprintf(&buf, "%d\n", int32(ab[0])|int32(ab[1])<<8|int32(ab[2])<<16|int32(ab[3])<<24)
	require.Equal(t, tc.output, buf.String())
}

// abi1: internal test() fills two :mem structs (via alpha) and passes them by
// value, on either side of nine ints, to the driver's fcb.
func testABI1(t *testing.T) {
	m, tc := loadABI(t, "abi1.ssa")

	var buf bytes.Buffer
	fcb := func(mc *interp.Machine, args []interp.Value) (interp.Value, error) {
		fmt.Fprintf(&buf, "fcb: m = (mem){ t = \"%s\" }\n", readCStr(mc, args[0].U64()))
		fmt.Fprintf(&buf, "     n = (mem){ t = \"%s\" }\n", readCStr(mc, args[10].U64()))
		for k := 1; k <= 9; k++ {
			fmt.Fprintf(&buf, "     i%d = %d\n", k, args[k].I64())
		}
		return interp.W(0), nil
	}
	mc, err := interp.New(m, interp.WithExtern("fcb", fcb))
	require.NoError(t, err)

	_, err = mc.Call("test")
	require.NoError(t, err)
	require.Equal(t, tc.output, buf.String())
}

// abi5: internal test() calls nine driver functions that each return a distinct
// struct by value, unpacks fields, and prints them. Each tN is an extern here,
// heap-allocating and filling the struct it returns.
func testABI5(t *testing.T) {
	m, tc := loadABI(t, "abi5.ssa")

	var buf bytes.Buffer
	agg := func(size int, fill func(mc *interp.Machine, p uint64)) interp.ExternFunc {
		return func(mc *interp.Machine, _ []interp.Value) (interp.Value, error) {
			p := mc.Alloc(size)
			fill(mc, p)
			return interp.Ptr(p), nil
		}
	}
	writeStr := func(mc *interp.Machine, p uint64, s string) {
		for i := 0; i < len(s); i++ {
			require.NoError(t, mc.Store(p+uint64(i), 1, uint64(s[i])))
		}
	}

	mc, err := interp.New(m, interp.WithStdout(&buf),
		interp.WithExtern("t1", agg(17, func(mc *interp.Machine, p uint64) { writeStr(mc, p, "abcdefghijklmnop") })),
		interp.WithExtern("t2", agg(4, func(mc *interp.Machine, p uint64) { mc.Store(p, 4, 2) })),
		interp.WithExtern("t3", agg(8, func(mc *interp.Machine, p uint64) { storeF32(t, mc, p, 3.0); mc.Store(p+4, 4, 30) })),
		interp.WithExtern("t4", agg(16, func(mc *interp.Machine, p uint64) { mc.Store(p, 4, 4); storeF64(t, mc, p+8, -40) })),
		interp.WithExtern("t5", agg(16, func(mc *interp.Machine, p uint64) {
			storeF32(t, mc, p, 5.5)
			storeI64(mc, p+8, -55)
		})),
		interp.WithExtern("t6", agg(16, func(mc *interp.Machine, p uint64) { writeStr(mc, p, "abcdefghijklmno") })),
		interp.WithExtern("t7", agg(16, func(mc *interp.Machine, p uint64) { storeF32(t, mc, p, 7.77); storeF64(t, mc, p+8, 77.7) })),
		interp.WithExtern("t8", agg(16, func(mc *interp.Machine, p uint64) {
			for i, v := range []int64{-8, 88, -888, 8888} {
				mc.Store(p+uint64(i*4), 4, uint64(uint32(v)))
			}
		})),
		interp.WithExtern("t9", agg(8, func(mc *interp.Machine, p uint64) { mc.Store(p, 4, 9); storeF32(t, mc, p+4, 9.9) })),
	)
	require.NoError(t, err)

	_, err = mc.Call("test")
	require.NoError(t, err)
	require.Equal(t, tc.output, buf.String())
}
