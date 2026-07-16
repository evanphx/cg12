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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// zeroModule builds a program using zero-filled storage of both kinds: zg is an
// ordinary global, ztls a thread-local, and tinit an initialized thread-local to
// copy from. main returns zg + ztls == 2 + 40. zeros adds that many bytes of
// further zero fill, which should cost nothing in the file.
func zeroModule(zeros int) *ir.Module {
	m := ir.NewModule()
	m.Data = append(m.Data,
		&ir.Data{Name: "zg", Align: 4, Items: []ir.DataItem{{Zero: 4}}},
		&ir.Data{Name: "ztls", Align: 4, Linkage: ir.Linkage{Thread: true},
			Items: []ir.DataItem{{Zero: 4}}},
		&ir.Data{Name: "tinit", Align: 4, Linkage: ir.Linkage{Thread: true},
			Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{40}}}},
	)
	if zeros > 0 {
		m.Data = append(m.Data, &ir.Data{Name: "huge", Align: 8,
			Items: []ir.DataItem{{Zero: zeros}}})
	}
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Store(f.Word(2), f.Sym("zg", 0))
	e.Store(e.Load(ir.ClsW, f.ThreadSym("tinit")), f.ThreadSym("ztls"))
	e.Ret(e.Add(ir.ClsW,
		e.Load(ir.ClsW, f.Sym("zg", 0)),
		e.Load(ir.ClsW, f.ThreadSym("ztls"))))
	return m
}

func linkZero(t *testing.T, zeros int, pie bool) []byte {
	t.Helper()
	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(zeroModule(zeros)))
	exe, err := l.LinkDynamicExecutableWith("main", obj.DynOptions{
		Needed: []string{"libc.so.6"}, PIE: pie,
	})
	require.NoError(t, err)
	return exe
}

// Zero-filled storage works the same as any other, for both a global and a
// thread-local: the loader supplies the zeroes rather than the file carrying them.
func TestZeroFilledStorage(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	for _, pie := range []bool{false, true} {
		name := "fixed-base"
		if pie {
			name = "pie"
		}
		t.Run(name, func(t *testing.T) {
			require.Equal(t, 42, runExe(t, linkZero(t, 0, pie))) // zg=2 + ztls=40
		})
	}
}

// The point of zero-filled sections: they cost addresses and run-time space but
// no file bytes at all. A megabyte of zeroes must not grow the image by a byte.
func TestZeroFilledStorageCostsNoFileSpace(t *testing.T) {
	small := linkZero(t, 0, false)
	huge := linkZero(t, 1<<20, false)
	assert.Equal(t, len(small), len(huge),
		"a megabyte of zero fill should add nothing to the file")

	f, err := elf.NewFile(bytes.NewReader(huge))
	require.NoError(t, err)
	for _, p := range f.Progs {
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_W != 0 {
			assert.Greater(t, p.Memsz, p.Filesz+(1<<20)-1,
				"the writable segment asks for the space at run time instead")
			return
		}
	}
	t.Fatal("no writable PT_LOAD")
}

// A thread's block is .tdata followed by .tbss, so PT_TLS asks for more memory
// than it carries in the file.
func TestThreadLocalBssExtendsTheTlsBlock(t *testing.T) {
	f, err := elf.NewFile(bytes.NewReader(linkZero(t, 0, false)))
	require.NoError(t, err)
	for _, p := range f.Progs {
		if p.Type == elf.PT_TLS {
			assert.Equal(t, uint64(4), p.Filesz, "only the initialized thread-local is carried")
			assert.Equal(t, uint64(8), p.Memsz, "the zero-filled one still needs room in the block")
			return
		}
	}
	t.Fatal("no PT_TLS segment")
}
