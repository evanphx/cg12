package lift_test

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/lift"
	"github.com/stretchr/testify/require"
)

// The foreign-code test: lift functions gcc compiled (not cg12), and check the
// lifted IR against the real gcc binary. There is no original IR to be the oracle
// here, so the oracle is the executable itself — compiled and run natively on this
// aarch64 host. It exercises idioms cg12 does not emit (csel, frameless leaves,
// gcc's cmp;b.cond loops).
func TestGccRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("gcc not available")
	}
	if !isARM64() {
		t.Skip("not an arm64 host; gcc would not emit arm64")
	}

	src := `
long add(long a, long b){ return a + b; }
long maxv(long a, long b){ return a > b ? a : b; }
long clampv(long x){ return x < 0 ? 0 : (x > 100 ? 100 : x); }
long absval(long x){ return x < 0 ? -x : x; }
long bits(long x){ return (x & 0xff) | ((x >> 16) & 0xff00); }
long shifts(long x, long n){ return (x << n) + (x >> n); }
long popc(long x){ long c = 0; unsigned long u = x; while (u) { c += u & 1; u >>= 1; } return c; }
long sumv(long n){ long s = 0; for (long i = 1; i <= n; i++) s += i; return s; }
long gcd(long a, long b){ while (b) { long r = a % b; a = b; b = r; } return a; }
long fib(long n){ if (n < 2) return n; return fib(n-1) + fib(n-2); }
`
	fns := []gccFn{
		{"add", 2}, {"maxv", 2}, {"clampv", 1}, {"absval", 1}, {"bits", 1},
		{"shifts", 2}, {"popc", 1}, {"sumv", 1}, {"gcd", 2}, {"fib", 1},
	}
	inputs := map[string][][]int64{
		"add":    {{3, 4}, {-1, 100}, {0, 0}},
		"maxv":   {{3, 4}, {9, 2}, {-1, -5}},
		"clampv": {{-7}, {50}, {200}},
		"absval": {{-9}, {12}, {0}},
		"bits":   {{0x12345678}, {-1}, {0}},
		"shifts": {{100, 3}, {-8, 2}, {1, 5}},
		"popc":   {{255}, {1024}, {0}, {-1}},
		"sumv":   {{10}, {100}, {1}, {0}},
		"gcd":    {{48, 36}, {100, 35}, {17, 5}},
		"fib":    {{0}, {1}, {10}, {15}},
	}

	for _, opt := range []string{"-O0", "-O1", "-O2"} {
		t.Run(opt, func(t *testing.T) { runGccSuite(t, opt, src, fns, inputs) })
	}
}

func runGccSuite(t *testing.T, opt, src string, fns []gccFn, inputs map[string][][]int64) {
	dir := t.TempDir()
	cfile := filepath.Join(dir, "g.c")
	require.NoError(t, os.WriteFile(cfile, []byte(src), 0644))

	obj := filepath.Join(dir, "g.o")
	gcc(t, opt, "-c", cfile, "-o", obj)

	exe := filepath.Join(dir, "g")
	drv := filepath.Join(dir, "d.c")
	require.NoError(t, os.WriteFile(drv, []byte(src+driverMain(fns)), 0644))
	gcc(t, opt, drv, "-o", exe)

	lifted := liftGcc(t, obj, fns)

	for _, fn := range fns {
		t.Run(fn.name, func(t *testing.T) {
			for _, in := range inputs[fn.name] {
				ref := runExe(t, exe, fn.name, in)
				mc, err := interp.New(freshCopy(t, lifted))
				require.NoError(t, err)
				args := make([]interp.Value, len(in))
				for i, v := range in {
					args[i] = interp.L(v)
				}
				res, err := mc.Call(fn.name, args...)
				require.NoErrorf(t, err, "interp %s(%v)", fn.name, in)
				require.Equalf(t, ref, res.I64(), "%s(%v): gcc %s binary vs interp(lifted)", fn.name, in, opt)
			}
		})
	}
}

type gccFn struct {
	name  string
	nargs int
}

// liftGcc extracts each function's machine code and bl relocations from the ELF
// object and lifts the whole set (so fib's self-call resolves).
func liftGcc(t *testing.T, objPath string, fns []gccFn) *ir.Module {
	t.Helper()
	ef, err := elf.Open(objPath)
	require.NoError(t, err)
	defer ef.Close()

	text := ef.Section(".text")
	data, err := text.Data()
	require.NoError(t, err)
	syms, err := ef.Symbols()
	require.NoError(t, err)
	relocs := textRelocs(t, ef, syms)

	var fcs []lift.FuncCode
	for _, fn := range fns {
		s := findSym(t, syms, fn.name)
		code := toWords(data[s.Value : s.Value+s.Size])
		calls := map[int]string{}
		for wi := range code {
			if name, ok := relocs[s.Value+uint64(wi*4)]; ok {
				calls[wi] = name
			}
		}
		params := make([]ir.Cls, fn.nargs)
		for i := range params {
			params[i] = ir.ClsL
		}
		fcs = append(fcs, lift.FuncCode{Name: fn.name, Code: code, Params: params, Retty: ir.ClsL, Calls: calls})
	}
	m, err := lift.LiftFuncs(fcs)
	require.NoError(t, err, "LiftFuncs")
	return m
}

// textRelocs maps each relocated .text offset to the target symbol name, reading
// .rela.text directly (a bl's CALL26 names its callee).
func textRelocs(t *testing.T, ef *elf.File, syms []elf.Symbol) map[uint64]string {
	t.Helper()
	rela := ef.Section(".rela.text")
	if rela == nil {
		return nil
	}
	data, err := rela.Data()
	require.NoError(t, err)
	out := map[uint64]string{}
	for i := 0; i+24 <= len(data); i += 24 {
		off := binary.LittleEndian.Uint64(data[i:])
		info := binary.LittleEndian.Uint64(data[i+8:])
		idx := int(info >> 32) // .symtab index; Symbols() omits the null entry 0
		if idx >= 1 && idx-1 < len(syms) {
			out[off] = syms[idx-1].Name
		}
	}
	return out
}

func findSym(t *testing.T, syms []elf.Symbol, name string) elf.Symbol {
	t.Helper()
	for _, s := range syms {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no symbol %q", name)
	return elf.Symbol{}
}

func toWords(b []byte) []uint32 {
	w := make([]uint32, len(b)/4)
	for i := range w {
		w[i] = uint32(b[i*4]) | uint32(b[i*4+1])<<8 | uint32(b[i*4+2])<<16 | uint32(b[i*4+3])<<24
	}
	return w
}

// driverMain generates a dispatcher that calls each function by name from argv.
func driverMain(fns []gccFn) string {
	var b strings.Builder
	b.WriteString("\n#include <stdio.h>\n#include <stdlib.h>\n#include <string.h>\nint main(int c, char**v){\n")
	for _, fn := range fns {
		var args []string
		for i := 0; i < fn.nargs; i++ {
			args = append(args, fmt.Sprintf("atol(v[%d])", i+2))
		}
		fmt.Fprintf(&b, "  if(!strcmp(v[1],%q)) printf(\"%%ld\\n\", %s(%s));\n", fn.name, fn.name, strings.Join(args, ","))
	}
	b.WriteString("  return 0;\n}\n")
	return b.String()
}

func runExe(t *testing.T, exe, fn string, args []int64) int64 {
	t.Helper()
	argv := []string{fn}
	for _, a := range args {
		argv = append(argv, strconv.FormatInt(a, 10))
	}
	out, err := exec.Command(exe, argv...).Output()
	require.NoErrorf(t, err, "run %s %v", fn, args)
	v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	require.NoError(t, err)
	return v
}

func gcc(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("gcc", args...).CombinedOutput(); err != nil {
		t.Fatalf("gcc %v: %v\n%s", args, err, out)
	}
}

func isARM64() bool {
	out, err := exec.Command("uname", "-m").Output()
	return err == nil && strings.TrimSpace(string(out)) == "aarch64"
}
