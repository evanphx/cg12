package arm64_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/require"
)

// Position-independent code. The question the backend has to answer at every
// symbol reference is whether the distance from the code to the symbol is fixed
// when the static linker runs. For a symbol this module defines it is, and a
// PC-relative adrp reaches it. For one that binds elsewhere it is not -- a
// position-independent executable is loaded wherever the kernel puts it -- and
// the linker refuses the adrp outright rather than emit something that would be
// wrong at run time:
//
//	relocation R_AARCH64_ADR_PREL_PG_HI21 against symbol `stdin@@GLIBC_2.17'
//	which may bind externally can not be used when making a shared object
//
// So under PIC an external symbol is reached through the global offset table,
// whose slot IS in this image, and which the loader fills with the real address.

// picModule builds probe(), which reads local_counter and external_counter, adds
// them, and writes the sum back to external_counter. One symbol the module
// defines, one it does not, reached by both a load and a store -- so a single
// compile covers every place a symbol's address is formed and can be compared
// against itself.
//
// The recorded alignment matters: it is what lets the selector fold the symbol's
// low bits into the access itself rather than materialize the address first.
// That is the shape the C front end produces, so it is the one worth testing.
func picModule() *ir.Module {
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{
		Name: "local_counter", Align: 4, Linkage: ir.Linkage{Export: true},
		Items: []ir.DataItem{{Sub: ir.SubW, Ints: []int64{7}}},
	})
	m.NoteSymAlign("local_counter", 4)
	m.NoteSymAlign("external_counter", 4)
	f := m.NewFunc("probe", ir.ClsW).Export()
	e := f.Entry()
	local := e.Load(ir.ClsW, f.Sym("local_counter", 0))
	external := e.Load(ir.ClsW, f.Sym("external_counter", 0))
	sum := e.Add(ir.ClsW, local, external)
	e.Store(sum, f.Sym("external_counter", 0))
	e.Ret(sum)
	return m
}

// linkFailsAsPIE reports whether the host linker refuses to make a
// position-independent executable out of data.
func linkFailsAsPIE(t *testing.T, data []byte) bool {
	t.Helper()
	cc := assembler(t)
	dir := t.TempDir()
	objPath := filepath.Join(dir, "u.o")
	cPath := filepath.Join(dir, "main.c")
	require.NoError(t, os.WriteFile(objPath, data, 0o644))
	require.NoError(t, os.WriteFile(cPath, []byte(`
extern int have_stdout(void);
int main(void){ return have_stdout() ? 0 : 1; }`), 0o644))
	out, err := exec.Command(cc, "-pie", "-fPIE", "-o", filepath.Join(dir, "prog"), objPath, cPath).CombinedOutput()
	if err != nil {
		t.Logf("link rejected it, as expected: %s", out)
	}
	return err != nil
}

// relocTypesFor returns the relocation types recorded against sym, in order.
func relocTypesFor(o *obj.Object, sym string) []uint32 {
	var types []uint32
	for _, r := range o.Relocs {
		if r.Sym == sym {
			types = append(types, r.Type)
		}
	}
	return types
}

// PIC splits the two references: the symbol this object defines keeps the
// PC-relative pair, and only the external one goes through the GOT. That split
// is the whole point -- routing everything through the GOT would cost a load on
// every global access for no benefit, since a local symbol cannot move relative
// to the code that reads it.
func TestPICUsesGOTOnlyForExternalSymbols(t *testing.T) {
	object, err := arm64.CompileToObjectWith(picModule(), arm64.Options{PIC: true})
	require.NoError(t, err)

	require.Equal(t, []uint32{
		obj.R_AARCH64_ADR_GOT_PAGE, obj.R_AARCH64_LD64_GOT_LO12_NC, // the load
		obj.R_AARCH64_ADR_GOT_PAGE, obj.R_AARCH64_LD64_GOT_LO12_NC, // the store
	}, relocTypesFor(object, "external_counter"),
		"a symbol this object does not define must be reached through the GOT")

	require.Equal(t,
		[]uint32{obj.R_AARCH64_ADR_PREL_PG_HI21, obj.R_AARCH64_LDST32_ABS_LO12_NC},
		relocTypesFor(object, "local_counter"),
		"a symbol this object defines is already position-independent")
}

// Without PIC both references keep the PC-relative form. This is the default,
// and it is what a caller linking -no-pie wants: one instruction fewer, and no
// GOT slot.
func TestWithoutPICEverySymbolIsPCRelative(t *testing.T) {
	object, err := arm64.CompileToObject(picModule())
	require.NoError(t, err)

	pcRelative := []uint32{obj.R_AARCH64_ADR_PREL_PG_HI21, obj.R_AARCH64_LDST32_ABS_LO12_NC}
	require.Equal(t, pcRelative, relocTypesFor(object, "local_counter"))
	require.Equal(t, append(append([]uint32{}, pcRelative...), pcRelative...),
		relocTypesFor(object, "external_counter"))
}

// The end of the argument: a PIE that actually runs. The relocation types above
// are a claim about what the linker will accept, and this is the linker
// accepting it -- the same module, linked -pie, reading a libc-adjacent global
// defined by the C driver and returning the right sum.
func TestPICObjectLinksAndRunsAsPIE(t *testing.T) {
	data, err := arm64.CompileObjectWith(picModule(), arm64.Options{PIC: true})
	require.NoError(t, err)
	code := runObjectCC(t, data, `
extern int local_counter;
int external_counter = 35;
extern int probe(void);
int main(void){ return (probe() == 42 && local_counter == 7) ? 0 : 1; }`, "-pie", "-fPIE")
	require.Equal(t, 0, code)
}

// libcDataModule builds have_stdout() = (stdout != 0).
//
// stdout is the case that actually breaks, and the reason is narrower than
// "external symbol". A symbol the executable defines itself moves WITH the code
// when a PIE is loaded -- the whole image slides by one offset -- so a
// PC-relative reference to it is fine even though the compiler had never seen a
// definition. Only a symbol that binds into ANOTHER image, like this one in
// libc, sits at a distance nothing knows until the loader runs.
func libcDataModule() *ir.Module {
	m := ir.NewModule()
	m.NoteSymAlign("stdout", 8)
	f := m.NewFunc("have_stdout", ir.ClsW).Export()
	e := f.Entry()
	stream := e.Load(ir.ClsL, f.Sym("stdout", 0))
	e.Ret(e.Cmp(ir.CmpNe, ir.ClsW, stream, f.Long(0)))
	return m
}

// The end of the argument: a PIE that actually runs, reading a global out of a
// shared library. The relocation types above are a claim about what the linker
// will accept; this is the linker accepting it.
func TestPICReachesSharedLibraryDataFromAPIE(t *testing.T) {
	data, err := arm64.CompileObjectWith(libcDataModule(), arm64.Options{PIC: true})
	require.NoError(t, err)
	code := runObjectCC(t, data, `
extern int have_stdout(void);
int main(void){ return have_stdout() ? 0 : 1; }`, "-pie", "-fPIE")
	require.Equal(t, 0, code)
}

// And the failure PIC exists to prevent. Without it the same module cannot be
// linked into a position-independent executable at all -- which is what made
// every cg12-compiled program that so much as named stdout need -no-pie.
func TestNonPICCannotReachSharedLibraryDataFromAPIE(t *testing.T) {
	data, err := arm64.CompileObject(libcDataModule())
	require.NoError(t, err)
	require.True(t, linkFailsAsPIE(t, data),
		"a PC-relative reference to a symbol in a shared library must not link "+
			"into a PIE; if this starts passing, the test above proves nothing")
}
