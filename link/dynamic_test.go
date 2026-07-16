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
	"github.com/evanphx/cg12/obj"
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

// Several exports in one library exercise the hash tables for real: the loader
// looks each name up through DT_GNU_HASH (which it prefers over DT_HASH when both
// are present), walking a bucket's chain to the right symbol. A wrong bucket
// order or end-of-chain marker would fail to resolve.
func TestSharedLibraryMultipleExports(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	m := ir.NewModule()
	for _, fn := range []struct {
		name string
		body func(f *ir.Func, e *ir.Block, x ir.Ref)
	}{
		{"cg12_add3", func(f *ir.Func, e *ir.Block, x ir.Ref) { e.Ret(e.Add(ir.ClsW, x, f.Word(3))) }},
		{"cg12_mul2", func(f *ir.Func, e *ir.Block, x ir.Ref) { e.Ret(e.Mul(ir.ClsW, x, f.Word(2))) }},
		{"cg12_neg", func(f *ir.Func, e *ir.Block, x ir.Ref) { e.Ret(e.Sub(ir.ClsW, f.Word(0), x)) }},
	} {
		f := m.NewFunc(fn.name, ir.ClsW).Export()
		x := f.Param("x", ir.ClsW)
		fn.body(f, f.Entry(), x)
	}
	exports := []string{"cg12_add3", "cg12_mul2", "cg12_neg"}

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(m))
	so, err := l.LinkSharedLibrary("libcg12multi.so", exports)
	require.NoError(t, err)
	dir := t.TempDir()
	soPath := filepath.Join(dir, "libcg12multi.so")
	require.NoError(t, os.WriteFile(soPath, so, 0o755))

	// main() = add3(mul2(neg(-10))) = add3(mul2(10)) = add3(20) = 23
	const rtldNow = 2
	p := ir.NewModule()
	p.Data = append(p.Data, &ir.Data{Name: "sopath", Align: 1,
		Items: []ir.DataItem{{Str: soPath}, {Sub: ir.SubB, Ints: []int64{0}}}})
	for _, n := range exports {
		p.Data = append(p.Data, &ir.Data{Name: "n_" + n, Align: 1,
			Items: []ir.DataItem{{Str: n}, {Sub: ir.SubB, Ints: []int64{0}}}})
	}
	f := p.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	h := e.Call(ir.ClsL, f.Sym("dlopen", 0), f.Sym("sopath", 0), f.Word(rtldNow))
	sym := func(n string) ir.Ref { return e.Call(ir.ClsL, f.Sym("dlsym", 0), h, f.Sym("n_"+n, 0)) }
	v := e.Call(ir.ClsW, sym("cg12_neg"), f.Word(-10))
	v = e.Call(ir.ClsW, sym("cg12_mul2"), v)
	v = e.Call(ir.ClsW, sym("cg12_add3"), v)
	e.Ret(v)

	l2 := link.NewWith(arm64.Backend{})
	require.NoError(t, l2.AddModule(p))
	exe, err := l2.LinkDynamicExecutable("main", "libc.so.6")
	require.NoError(t, err)
	require.Equal(t, 23, runExe(t, exe))
}

// An import can be pinned to a specific version of the library that defines it,
// rather than binding to whatever that library marks as the default.
func TestVersionedImport(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	build := func(version string) ([]byte, error) {
		l := link.NewWith(arm64.Backend{})
		require.NoError(t, l.AddModule(moduleCallsAbs()))
		return l.LinkDynamicExecutableWith("main", obj.DynOptions{
			Needed:  []string{"libc.so.6"},
			Require: map[string]obj.SymVersion{"abs": {Library: "libc.so.6", Version: version}},
		})
	}

	t.Run("binds the requested version", func(t *testing.T) {
		exe, err := build("GLIBC_2.17")
		require.NoError(t, err)
		require.Equal(t, 5, runExe(t, exe)) // abs(-5)
	})

	// The requirement is a real constraint, not a decoration: demanding a version
	// the library does not carry must make the loader refuse to start the program.
	t.Run("a version the library lacks is refused", func(t *testing.T) {
		exe, err := build("GLIBC_9.99")
		require.NoError(t, err, "linking still succeeds; the loader is what objects")
		require.NotEqual(t, 5, runExe(t, exe))
	})
}

// Lazy binding resolves an import on its first call instead of at load time.
// Calling two imports (one of them twice) covers both the resolver trampoline and
// the already-resolved path it leaves behind.
func TestLazyBinding(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	for _, pie := range []bool{false, true} {
		name := "fixed-base"
		if pie {
			name = "pie"
		}
		t.Run(name, func(t *testing.T) {
			m := ir.NewModule()
			f := m.NewFunc("main", ir.ClsW).Export()
			e := f.Entry()
			a := e.Call(ir.ClsW, f.Sym("abs", 0), f.Word(-5))
			b := e.Call(ir.ClsW, f.Sym("labs", 0), f.Long(-30))
			c := e.Call(ir.ClsW, f.Sym("abs", 0), f.Word(-7)) // second call: already bound
			e.Ret(e.Add(ir.ClsW, e.Add(ir.ClsW, a, b), c))    // 5 + 30 + 7 = 42

			l := link.NewWith(arm64.Backend{})
			require.NoError(t, l.AddModule(m))
			exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{
				Needed: []string{"libc.so.6"}, Lazy: true, PIE: pie,
			})
			require.NoError(t, err)
			require.Equal(t, 42, runExe(t, exe))
		})
	}
}

// Lazy binding is really lazy, and this is what proves it rather than asserting on
// a flag: a program that references a symbol nothing defines, but never calls it,
// runs fine when bound lazily and dies at startup when bound eagerly.
func TestLazyBindingDefersUnusedImport(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	build := func(lazy bool) []byte {
		m := ir.NewModule()
		// g is zero, so the call below is emitted but never reached.
		m.Data = append(m.Data, &ir.Data{Name: "g", Align: 4,
			Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{0}}}})
		f := m.NewFunc("main", ir.ClsW).Export()
		e := f.Entry()
		call, done := f.NewBlock("call"), f.NewBlock("done")
		e.Jnz(e.Load(ir.ClsW, f.Sym("g", 0)), call, done)
		call.Ret(call.Call(ir.ClsW, f.Sym("cg12_no_such_symbol", 0)))
		done.Ret(f.Word(42))

		l := link.NewWith(arm64.Backend{})
		require.NoError(t, l.AddModule(m))
		exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{
			Needed: []string{"libc.so.6"}, Lazy: lazy,
		})
		require.NoError(t, err)
		return exe
	}
	require.Equal(t, 42, runExe(t, build(true)), "lazy: never called, so never resolved")
	require.NotEqual(t, 42, runExe(t, build(false)), "eager: resolving it at load must fail")
}

// dynamicSegmentAddr returns the virtual address of an image's PT_DYNAMIC.
func dynamicSegmentAddr(t *testing.T, exe []byte) uint64 {
	t.Helper()
	f, err := elf.NewFile(bytes.NewReader(exe))
	require.NoError(t, err)
	for _, p := range f.Progs {
		if p.Type == elf.PT_DYNAMIC {
			return p.Vaddr
		}
	}
	t.Fatal("no PT_DYNAMIC segment")
	return 0
}

// Binding eagerly means every GOT slot is written before the image's own code
// runs, so the loader can freeze the GOT and .dynamic afterwards. That is the
// whole point of eager binding, and what proves it happened is behaviour rather
// than the presence of a segment: writing into the region faults when bound
// eagerly and succeeds when bound lazily, which needs the GOT writable.
func TestRelroFreezesTheGOT(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	// poke builds a program that stores to addr. Its shape does not depend on the
	// address, so a first build reveals where .dynamic lands and a second aims at it.
	poke := func(lazy bool, addr int64) []byte {
		m := ir.NewModule()
		f := m.NewFunc("main", ir.ClsW).Export()
		e := f.Entry()
		e.Store(f.Word(1), f.Long(addr))
		e.Ret(f.Word(0))
		l := link.NewWith(arm64.Backend{})
		require.NoError(t, l.AddModule(m))
		exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{
			Needed: []string{"libc.so.6"}, Lazy: lazy,
		})
		require.NoError(t, err)
		return exe
	}
	aimed := func(lazy bool) []byte {
		return poke(lazy, int64(dynamicSegmentAddr(t, poke(lazy, 0))))
	}

	const sigsegv = 139
	require.Equal(t, sigsegv, runExe(t, aimed(false)), "eager: .dynamic must be read-only after relocation")
	require.Equal(t, 0, runExe(t, aimed(true)), "lazy: the resolver needs it writable, so no relro")
}

// Relro must not overreach: the loader freezes whole pages, so .data has to start
// on one of its own. If it were caught in the region, this store would fault.
func TestRelroLeavesDataWritable(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	for _, pie := range []bool{false, true} {
		name := "fixed-base"
		if pie {
			name = "pie"
		}
		t.Run(name, func(t *testing.T) {
			m := ir.NewModule()
			m.Data = append(m.Data, &ir.Data{Name: "g", Align: 4,
				Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{0}}}})
			f := m.NewFunc("main", ir.ClsW).Export()
			e := f.Entry()
			e.Store(f.Word(41), f.Sym("g", 0))
			e.Ret(e.Add(ir.ClsW, e.Load(ir.ClsW, f.Sym("g", 0)), f.Word(1)))

			l := link.NewWith(arm64.Backend{})
			require.NoError(t, l.AddModule(m))
			exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{
				Needed: []string{"libc.so.6"}, PIE: pie,
			})
			require.NoError(t, err)
			require.Equal(t, 42, runExe(t, exe))
		})
	}
}

// A program can find its libraries by soname instead of by absolute path: the
// library is listed as NEEDED and DT_RUNPATH says where to look, with $ORIGIN
// standing for the directory the program itself was loaded from. That is how a
// program ships libraries beside it and stays relocatable on disk.
func TestRunpathFindsLibraryBySoname(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	dir := t.TempDir()
	buildTripleSo(t, dir) // libcg12demo.so, exporting cg12_triple

	// main() = cg12_triple(14); the symbol comes from the library, found via RUNPATH.
	m := ir.NewModule()
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Ret(e.Call(ir.ClsW, f.Sym("cg12_triple", 0), f.Word(14)))

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(m))
	exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{
		Needed:  []string{"libcg12demo.so", "libc.so.6"},
		Runpath: []string{"$ORIGIN"},
	})
	require.NoError(t, err)

	// Place the program beside the library so $ORIGIN resolves to their directory.
	path := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(path, exe, 0o755))
	err = exec.Command(path).Run()
	var code int
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else {
		require.NoError(t, err)
	}
	require.Equal(t, 42, code, "cg12_triple(14) resolved from the library next to the program")
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
