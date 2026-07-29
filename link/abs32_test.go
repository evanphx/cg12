package link_test

import (
	"runtime"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/link"
	"github.com/stretchr/testify/require"
)

// A four-byte data item naming a symbol emits R_AARCH64_ABS32, and the static
// image writer had no case for it: every such reference came back as `cannot
// statically resolve aarch64 relocation type 258`. goc emits thousands of them
// into the Go metadata tables, so nothing it produces could be linked without an
// external linker. The truncation is exact here -- the image is a fixed-base
// ET_EXEC at 0x400000, well below 4 GiB.
func TestStaticExecutableResolvesAbs32(t *testing.T) {
	if runtime.GOARCH != "arm64" {
		t.Skip("arm64 executable runs natively only on an arm64 host")
	}
	module := ir.NewModule()
	module.Data = append(module.Data,
		&ir.Data{Name: "value", Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{99}}}},
		// A 32-bit slot holding the address of "value", and an 8-bit one holding
		// the whole address, so the test can compare the two.
		&ir.Data{Name: "narrow", Align: 4, Items: []ir.DataItem{{Sub: ir.SubW, Sym: "value"}}},
		&ir.Data{Name: "wide", Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Sym: "value"}}},
	)
	// main returns 0 when the truncated pointer equals the low 32 bits of the
	// full one, and when loading through it finds the value it should.
	function := module.NewFunc("main", ir.ClsW).Export()
	entry := function.Entry()
	narrow := entry.Load(ir.ClsW, function.Sym("narrow", 0))
	wide := entry.Load(ir.ClsL, function.Sym("wide", 0))
	truncated := entry.Copy(ir.ClsW, wide)
	addressMismatch := entry.Sub(ir.ClsW, narrow, truncated)
	loaded := entry.Load(ir.ClsW, wide)
	valueMismatch := entry.Sub(ir.ClsW, loaded, function.Word(99))
	entry.Ret(entry.Or(ir.ClsW, addressMismatch, valueMismatch))

	linker := link.NewWith(arm64.Backend{})
	require.NoError(t, linker.AddModule(module))
	image, err := linker.LinkExecutable("main")
	require.NoError(t, err, "a static executable with a 32-bit symbol reference must link")
	require.Equal(t, 0, runExe(t, image), "the 32-bit reference names the same address as the 64-bit one")
}
