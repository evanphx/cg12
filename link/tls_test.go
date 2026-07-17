package link_test

import (
	"bytes"
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/link"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/require"
)

// tlsModule builds a program with two thread-locals of its own: counter starts at
// 41 and other at 100. main returns ++counter, plus other when readOther is set --
// which only comes out right if the two land at different offsets in the block.
func tlsModule(readOther bool) *ir.Module {
	m := ir.NewModule()
	m.Data = append(m.Data,
		&ir.Data{Name: "tls_counter", Align: 4, Linkage: ir.Linkage{Thread: true},
			Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{41}}}},
		&ir.Data{Name: "tls_other", Align: 4, Linkage: ir.Linkage{Thread: true},
			Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{100}}}},
	)
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	addr := f.ThreadSym("tls_counter")
	v := e.Add(ir.ClsW, e.Load(ir.ClsW, addr), f.Word(1)) // ++counter
	e.Store(v, addr)
	if readOther {
		v = e.Add(ir.ClsW, v, e.Load(ir.ClsW, f.ThreadSym("tls_other")))
	}
	e.Ret(v)
	return m
}

// A program can define and use its own thread-local storage: the linker lays the
// initialization image out in PT_TLS, and each access resolves to a fixed offset
// from the thread pointer. The loader builds the thread's block from that image
// before the program runs, so counter reads back the 41 it was initialized with.
func TestThreadLocalStorage(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	build := func(t *testing.T, pie, readOther bool) []byte {
		l := link.NewWith(arm64.Backend{})
		require.NoError(t, l.AddModule(tlsModule(readOther)))
		exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{
			Needed: []string{"libc.so.6"}, PIE: pie,
		})
		require.NoError(t, err)
		return exe
	}

	for _, pie := range []bool{false, true} {
		name := "fixed-base"
		if pie {
			name = "pie"
		}
		t.Run(name, func(t *testing.T) {
			require.Equal(t, 42, runExe(t, build(t, pie, false)), "++counter")
			// Both thread-locals at once: 42 + 100. A shared or overlapping offset
			// would not add up.
			require.Equal(t, 142, runExe(t, build(t, pie, true)), "++counter + other")
		})
	}

	t.Run("carries a PT_TLS segment", func(t *testing.T) {
		f, err := elf.NewFile(bytes.NewReader(build(t, false, true)))
		require.NoError(t, err)
		var tls *elf.Prog
		for _, p := range f.Progs {
			if p.Type == elf.PT_TLS {
				tls = p
			}
		}
		require.NotNil(t, tls, "the thread-local initialization image is described by PT_TLS")
		require.Equal(t, uint64(8), tls.Memsz, "two 4-byte thread-locals")
		require.Equal(t, uint64(4), tls.Align)
	})
}

// A static executable has no loader to set up a thread pointer, so thread-local
// storage cannot work there. Say so at link time rather than emitting an image
// that reads from a register nothing ever set.
func TestThreadLocalInStaticExecutableErrors(t *testing.T) {
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(tlsModule(false)))
	_, err := l.LinkExecutable("main")
	require.Error(t, err)
	require.Contains(t, err.Error(), "thread")
}

// Only local-exec TLS is supported: the variable has to be in this image. A
// thread-local that lives in some library needs a model we do not emit yet, so
// that is a clear error rather than a wrong offset.
func TestThreadLocalFromLibraryErrors(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Ret(e.Load(ir.ClsW, f.ThreadSym("errno"))) // defined in libc, not here

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(m))
	_, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{Needed: []string{"libc.so.6"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "local-exec")
}

// tlsLibModule builds a shared library with a thread-local of its own:
// lib_bump() returns ++libtls, starting from 41.
func tlsLibModule() *ir.Module {
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{Name: "libtls", Align: 4, Linkage: ir.Linkage{Thread: true},
		Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{41}}}})
	f := m.NewFunc("lib_bump", ir.ClsW).Export()
	e := f.Entry()
	a := f.ThreadSym("libtls")
	v := e.Add(ir.ClsW, e.Load(ir.ClsW, a), f.Word(1))
	e.Store(v, a)
	e.Ret(v)
	return m
}

// A shared library can have thread-local storage, which local-exec cannot express:
// the library's block is placed by the loader, so the offset from the thread
// pointer is not a link-time constant. Initial-exec reads that offset from a GOT
// slot the loader fills instead.
func TestInitialExecTLSInSharedLibrary(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	dir := t.TempDir()

	l := link.NewWith(arm64.Backend{Opts: arm64.Options{TLSModel: arm64.TLSInitialExec}})
	require.NoError(t, l.AddModule(tlsLibModule()))
	so, err := l.LinkSharedLibraryWith(obj.SharedOptions{
		Soname: "libietls.so", Export: []string{"lib_bump"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "libietls.so"), so, 0o755))

	// The GOT slot is filled by the loader, which alone knows where the library's
	// block landed; the relocation only names the variable.
	f, err := elf.NewFile(bytes.NewReader(so))
	require.NoError(t, err)
	require.NotNil(t, f.Section(".dynsym"), "the thread-local must be a dynamic symbol to be named")

	p := ir.NewModule()
	g := p.NewFunc("main", ir.ClsW).Export()
	g.Entry().Ret(g.Entry().Call(ir.ClsW, g.Sym("lib_bump", 0)))
	l2 := link.NewWith(arm64.Backend{})
	require.NoError(t, l2.AddModule(p))
	exe, err := l2.LinkDynamicExecutableWith("main", obj.DynOptions{
		Needed: []string{"libietls.so", "libc.so.6"}, Runpath: []string{"$ORIGIN"},
	})
	require.NoError(t, err)
	path := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(path, exe, 0o755))

	err = exec.Command(path).Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else {
		require.NoError(t, err)
	}
	require.Equal(t, 42, code, "++libtls, reached from inside the library")
}

// General-dynamic asks __tls_get_addr for the address rather than working it out
// from the thread pointer, so it serves a thread-local whose storage may not
// exist in this thread yet -- one in a library brought in by dlopen, which is
// past the point where initial-exec's static block was sized.
//
// The test asserts general-dynamic works, and deliberately does not try to show
// initial-exec failing here: glibc keeps a surplus of static TLS, so initial-exec
// often gets away with it, and a test resting on that would be flaky.
func TestGeneralDynamicTLSInDlopenedLibrary(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	cc := toolchain(t)
	dir := t.TempDir()

	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{Name: "gdtls", Align: 4, Linkage: ir.Linkage{Thread: true},
		Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{41}}}})
	f := m.NewFunc("gd_bump", ir.ClsW).Export()
	e := f.Entry()
	a := f.ThreadSym("gdtls")
	v := e.Add(ir.ClsW, e.Load(ir.ClsW, a), f.Word(1))
	e.Store(v, a)
	e.Ret(v)

	l := link.NewWith(arm64.Backend{Opts: arm64.Options{TLSModel: arm64.TLSGeneralDynamic}})
	require.NoError(t, l.AddModule(m))
	so, err := l.LinkSharedLibraryWith(obj.SharedOptions{
		Soname: "libgdtls.so", Export: []string{"gd_bump"},
		Needed: []string{"libc.so.6"}, // where __tls_get_addr is found
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "libgdtls.so"), so, 0o755))

	// A C host so the library really is brought in late, by dlopen.
	src := filepath.Join(dir, "host.c")
	require.NoError(t, os.WriteFile(src, []byte(`
#include <dlfcn.h>
int main(void){
  void *h = dlopen("./libgdtls.so", RTLD_NOW);
  if(!h) return 1;
  int (*bump)(void) = dlsym(h, "gd_bump");
  if(!bump) return 2;
  return bump();
}`), 0o644))
	bin := filepath.Join(dir, "host")
	out, err := exec.Command(cc, "-o", bin, src).CombinedOutput()
	require.NoErrorf(t, err, "building the host: %s", out)

	cmd := exec.Command(bin)
	cmd.Dir = dir
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else {
		require.NoError(t, err)
	}
	require.Equal(t, 42, code, "++gdtls in a library dlopened after startup")
}
