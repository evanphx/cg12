package link_test

import (
	"runtime"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/link"
	"github.com/stretchr/testify/require"
)

// A static executable with a zero-initialized global is ordinary C, and the
// static image writer had no case for .bss at all: the symbol was never given an
// address, so it came back as `undefined symbol "counter" referenced by
// relocation`. Only the dynamic writer knew about .bss, and link/bss_test.go
// only exercises that one.
func TestStaticExecutablePlacesBss(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	m := ir.NewModule()
	m.Data = append(m.Data,
		&ir.Data{Name: "msg", Align: 1, Items: []ir.DataItem{{Str: "abcde\x00\x00"}}}, // .data, 7: ends odd
		&ir.Data{Name: "counter", Align: 4, Items: []ir.DataItem{{Zero: 4}}},          // .bss
		&ir.Data{Name: "wide", Align: 16, Items: []ir.DataItem{{Zero: 16}}},           // .bss, stricter
	)
	// main writes through both .bss words and reads them back, so they must be
	// mapped and writable; it returns their sum plus the zeroes it started with.
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Store(f.Word(40), f.Sym("counter", 0))
	e.Store(f.Word(2), f.Sym("wide", 0))
	got := e.Add(ir.ClsW, e.Load(ir.ClsW, f.Sym("counter", 0)), e.Load(ir.ClsW, f.Sym("wide", 0)))
	e.Ret(e.Sub(ir.ClsW, got, f.Word(42))) // 0 when both round-tripped

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(m))
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err, "a static executable with .bss must link")
	require.Equal(t, 0, runExe(t, exe), "the .bss words are mapped, zeroed, and writable")
}

// A program whose only writable data is .bss still needs the writable segment:
// it was created only when .data had bytes.
func TestStaticExecutableWithOnlyBss(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{Name: "only", Align: 4, Items: []ir.DataItem{{Zero: 4}}})
	f := m.NewFunc("main", ir.ClsW).Export()
	e := f.Entry()
	e.Store(f.Word(7), f.Sym("only", 0))
	e.Ret(e.Sub(ir.ClsW, e.Load(ir.ClsW, f.Sym("only", 0)), f.Word(7)))

	l := link.NewWith(arm64.Backend{})
	require.NoError(t, l.AddModule(m))
	exe, err := l.LinkExecutable("main")
	require.NoError(t, err)
	require.Equal(t, 0, runExe(t, exe), "a bss-only program is mapped somewhere real")
}
