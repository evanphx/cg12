package link_test

import (
	"bytes"
	"debug/elf"
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
