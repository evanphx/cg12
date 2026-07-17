package link_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/link"
)

// gccObj compiles C to a native AArch64 .o with the host or cross toolchain.
func gccObj(t *testing.T, src string, flags ...string) []byte {
	t.Helper()
	cc := toolchain(t)
	dir := t.TempDir()
	c := filepath.Join(dir, "x.c")
	o := filepath.Join(dir, "x.o")
	require.NoError(t, os.WriteFile(c, []byte(src), 0o644))
	argv := append([]string{"-w", "-c", "-o", o}, flags...)
	out, err := exec.Command(cc, append(argv, c)...).CombinedOutput()
	require.NoErrorf(t, err, "cc: %s", out)
	data, err := os.ReadFile(o)
	require.NoError(t, err)
	return data
}

// A gcc object containing a string literal links and runs.
//
// A literal has no name for a relocation to refer to, so gcc points at the
// section instead: `.rodata.str1.8 + 0`, a relocation against a section symbol.
// ReadELF dropped section symbols, so those relocations came back naming nothing
// -- and this is the most ordinary C there is, which made it the shape most
// likely to be the first foreign object anyone fed the linker.
//
// The program returns a byte fetched through the literal's address, so it is
// wrong unless the relocation resolved to the right place: it is the relocation
// that computes the address at all.
func TestLinkGccObjectWithStringLiteral(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	// msg() hands back the literal's address, which gcc cannot fold away: it must
	// compute it, and the only way to name an unnamed literal is a relocation
	// against the section holding it. (Reading the byte *inside* the object would
	// prove nothing -- gcc folds `msg[0]` to `mov w0, #0x41` and emits no
	// relocation at all, so the test would pass without ever exercising this.)
	foreign := gccObj(t, `const char *msg(void) { return "ABC"; }`, "-O2")

	m := ir.NewModule()
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Ret(e.LoadSub(ir.ClsW, ir.SubUB, e.Call(ir.ClsL, f.Sym("msg", 0))))

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(m))
	require.NoError(t, l.AddObjectFile(foreign), "reading a gcc object with a string literal")
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err, "linking against a gcc object with a string literal")
	require.Equal(t, 'A', rune(runExe(t, exe)), `first() reads msg[0] == 'A'`)
}

// Two literals in separate sections, reached from separate functions: each
// section symbol has to name its own section, at its own base in the merged
// .rodata. One name for both puts every literal on the same bytes -- which links
// cleanly and reads the wrong string.
func TestLinkGccObjectDistinguishesSections(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	// -fdata-sections gives each datum its own section, so the two tables below
	// land in different ones and must not be confused for each other.
	foreign := gccObj(t, `
		static const char a[] = "ab";
		static const char b[] = "yz";
		const char *pa(void) { return a; }
		const char *pb(void) { return b; }
	`, "-O2", "-fdata-sections")

	m := ir.NewModule()
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	// pa()[0] + pb()[0] == 'a' + 'y' == 218; both wrong if the sections are mixed.
	x := e.LoadSub(ir.ClsW, ir.SubUB, e.Call(ir.ClsL, f.Sym("pa", 0)))
	y := e.LoadSub(ir.ClsW, ir.SubUB, e.Call(ir.ClsL, f.Sym("pb", 0)))
	e.Ret(e.Add(ir.ClsW, x, y))

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(m))
	require.NoError(t, l.AddObjectFile(foreign))
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err)
	require.Equal(t, int('a')+int('y'), runExe(t, exe), "each datum keeps its own address")
}
