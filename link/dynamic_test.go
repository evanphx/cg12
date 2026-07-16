package link_test

import (
	"bytes"
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/link"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moduleCallsAbs builds main() = abs(-5), calling libc's abs through the PLT.
func moduleCallsAbs() *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Ret(e.Call(ir.ClsW, f.Sym("abs", 0), f.Word(-5)))
	return m
}

// moduleCallsStrlen builds main() = strlen("hello"), which passes a pointer to a
// module-level string: an imported call and a global-data reference together.
func moduleCallsStrlen() *ir.Module {
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{
		Name:  "msg",
		Align: 1,
		Items: []ir.DataItem{{Str: "hello"}, {Sub: ir.SubB, Ints: []int64{0}}},
	})
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Ret(e.Call(ir.ClsW, f.Sym("strlen", 0), f.Sym("msg", 0)))
	return m
}

// A dynamically linked executable calls into libc: the linker imports abs, binds
// it at load time through a synthesized PLT stub and GOT slot, and the running
// program returns the library function's result.
func TestDynamicExecutableCallsLibc(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("dynamic arm64 executable runs natively only on an arm64 host")
	}
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(moduleCallsAbs()))
	exe, err := l.LinkDynamicExecutable("main", "libc.so.6")
	require.NoError(t, err)
	require.Equal(t, 5, runExe(t, exe)) // abs(-5)
}

// An imported call that takes a pointer into our own .data exercises the import
// and the global-data relocations in one program.
func TestDynamicExecutablePassesPointerToLibc(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("dynamic arm64 executable runs natively only on an arm64 host")
	}
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(moduleCallsStrlen()))
	exe, err := l.LinkDynamicExecutable("main", "libc.so.6")
	require.NoError(t, err)
	require.Equal(t, 5, runExe(t, exe)) // strlen("hello")
}

// Two distinct imports each get their own PLT stub and GOT slot, so the
// per-import indexing is exercised beyond the first entry.
func TestDynamicExecutableMultipleImports(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("dynamic arm64 executable runs natively only on an arm64 host")
	}
	m := ir.NewModule()
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	// abs(-5) + (int)labs(-37) = 42
	a := e.Call(ir.ClsW, f.Sym("abs", 0), f.Word(-5))
	b := e.Call(ir.ClsW, f.Sym("labs", 0), f.Long(-37))
	e.Ret(e.Add(ir.ClsW, a, b))

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(m))
	exe, err := l.LinkDynamicExecutable("main", "libc.so.6")
	require.NoError(t, err)
	require.Equal(t, 42, runExe(t, exe))
}

// moduleDerefsPointerInData builds main() = **p, where the module-level pointer p
// holds the address of the global g. The stored address is absolute, so a
// position-independent image cannot bind it at link time -- it becomes a RELATIVE
// relocation the loader rebases to wherever the image lands.
func moduleDerefsPointerInData(v int64) *ir.Module {
	m := ir.NewModule()
	m.Data = append(m.Data,
		&ir.Data{Name: "g", Align: 4, Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{v}}}},
		&ir.Data{Name: "p", Align: 8, Items: []ir.DataItem{{Sym: "g"}}}, // p = &g
	)
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Ret(e.Load(ir.ClsW, e.Load(ir.ClsL, f.Sym("p", 0))))
	return m
}

// A position-independent executable runs: the loader places it at an arbitrary
// base and rebases the pointer stored in .data via its RELATIVE relocation, so
// dereferencing that pointer still finds the global.
func TestPIEExecutable(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(moduleDerefsPointerInData(77)))
	exe, err := l.LinkPIE("main", "libc.so.6")
	require.NoError(t, err)

	f, err := elf.NewFile(bytes.NewReader(exe))
	require.NoError(t, err)
	require.Equal(t, elf.ET_DYN, f.Type, "a PIE is an ET_DYN image")

	require.Equal(t, 77, runExe(t, exe))
}

// moduleTripleLib builds a library exporting cg12_triple(x) = x*3.
func moduleTripleLib() *ir.Module {
	m := ir.NewModule()
	f := m.NewFunc("cg12_triple", ir.ClsW).Export()
	x := f.Param("x", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Mul(ir.ClsW, x, f.Word(3)))
	return m
}

// buildTripleSo links the demo shared library and writes it into dir.
func buildTripleSo(t *testing.T, dir string) string {
	t.Helper()
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(moduleTripleLib()))
	so, err := l.LinkSharedLibrary("libcg12demo.so", []string{"cg12_triple"})
	require.NoError(t, err)
	path := filepath.Join(dir, "libcg12demo.so")
	require.NoError(t, os.WriteFile(path, so, 0o755))
	return path
}

// Our shared library is a real one: a C program compiled by the system toolchain
// links against it and calls into it. This exercises the parts a loadable image
// alone does not need -- the section headers and dynamic symbol table the static
// linker reads to resolve cg12_triple at link time.
func TestSharedLibraryLinkedFromC(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 shared library links natively only on an arm64 host")
	}
	cc, ok := toolchain()
	if !ok {
		t.Skip("no AArch64 toolchain available")
	}
	dir := t.TempDir()
	buildTripleSo(t, dir)

	src := filepath.Join(dir, "main.c")
	require.NoError(t, os.WriteFile(src, []byte(`
extern int cg12_triple(int);
int main(void){ return cg12_triple(14) == 42 ? 0 : 1; }`), 0o644))

	bin := filepath.Join(dir, "useso")
	out, err := exec.Command(cc, "-o", bin, src, "-L"+dir, "-l:libcg12demo.so", "-Wl,-rpath,"+dir).CombinedOutput()
	require.NoErrorf(t, err, "linking against our .so failed: %s", out)
	require.NoError(t, exec.Command(bin).Run(), "the C program calling our library should succeed")
}

// The whole round trip with no external toolchain at all: a cg12-built program
// dlopens a cg12-built shared library, resolves a symbol from it with dlsym, and
// calls it through the returned pointer.
func TestDlopenOwnSharedLibrary(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	dir := t.TempDir()
	soPath := buildTripleSo(t, dir)

	// main() { return ((int(*)(int))dlsym(dlopen(path, RTLD_NOW), "cg12_triple"))(14); }
	const rtldNow = 2
	m := ir.NewModule()
	m.Data = append(m.Data,
		&ir.Data{Name: "sopath", Align: 1, Items: []ir.DataItem{{Str: soPath}, {Sub: ir.SubB, Ints: []int64{0}}}},
		&ir.Data{Name: "symname", Align: 1, Items: []ir.DataItem{{Str: "cg12_triple"}, {Sub: ir.SubB, Ints: []int64{0}}}},
	)
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	h := e.Call(ir.ClsL, f.Sym("dlopen", 0), f.Sym("sopath", 0), f.Word(rtldNow))
	fp := e.Call(ir.ClsL, f.Sym("dlsym", 0), h, f.Sym("symname", 0))
	e.Ret(e.Call(ir.ClsW, fp, f.Word(14))) // indirect call through the dlsym result

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(m))
	exe, err := l.LinkDynamicExecutable("main", "libc.so.6")
	require.NoError(t, err)
	require.Equal(t, 42, runExe(t, exe)) // cg12_triple(14)
}

// The image is a well-formed dynamic executable on both architectures: it names
// the loader in PT_INTERP and carries a PT_DYNAMIC segment. This runs everywhere,
// including where the foreign architecture's libraries are not installed.
func TestDynamicExecutableStructure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		be      link.Backend
		machine elf.Machine
		interp  string
	}{
		{"arm64", arm64.Backend{}, elf.EM_AARCH64, "/lib/ld-linux-aarch64.so.1"},
		{"amd64", amd64.Backend{}, elf.EM_X86_64, "/lib64/ld-linux-x86-64.so.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := link.NewWith(tc.be)
			require.NoError(t, l.AddModule(moduleCallsAbs()))
			exe, err := l.LinkDynamicExecutable("main", "libc.so.6")
			require.NoError(t, err)

			f, err := elf.NewFile(bytes.NewReader(exe))
			require.NoError(t, err)
			assert.Equal(t, elf.ET_EXEC, f.Type)
			assert.Equal(t, tc.machine, f.Machine)

			var interp string
			var haveDynamic bool
			for _, p := range f.Progs {
				switch p.Type {
				case elf.PT_INTERP:
					b := make([]byte, p.Filesz)
					_, err := p.ReadAt(b, 0)
					require.NoError(t, err)
					interp = string(bytes.TrimRight(b, "\x00"))
				case elf.PT_DYNAMIC:
					haveDynamic = true
				}
			}
			assert.Equal(t, tc.interp, interp, "names the dynamic loader")
			assert.True(t, haveDynamic, "has a PT_DYNAMIC segment")
		})
	}
}
