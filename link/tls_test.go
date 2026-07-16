package link_test

import (
	"bytes"
	"debug/elf"
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
