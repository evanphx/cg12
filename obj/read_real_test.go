package obj_test

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/require"
)

// gccObject compiles C with the host gcc and returns the .o bytes.
func gccObject(t *testing.T, src string) []byte {
	t.Helper()
	gcc := testenv.Tool(t, "gcc")
	dir := t.TempDir()
	c := filepath.Join(dir, "x.c")
	o := filepath.Join(dir, "x.o")
	require.NoError(t, os.WriteFile(c, []byte(src), 0o644))
	out, err := exec.Command(gcc, "-w", "-c", "-o", o, c).CombinedOutput()
	require.NoErrorf(t, err, "gcc: %s", out)
	data, err := os.ReadFile(o)
	require.NoError(t, err)
	return data
}

func symOf(t *testing.T, o *obj.Object, name string) obj.Sym {
	t.Helper()
	for _, s := range o.Syms {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no symbol %q in %v", name, o.Syms)
	return obj.Sym{}
}

// Reading a real toolchain's object is what ReadELF is for, and a real object
// has more sections than our model. Each of these globals sits at offset 0 of a
// different section, so mapping several sections onto one without rebasing puts
// them all on the same byte -- which links cleanly and reads the wrong variable.
func TestReadRealObjectKeepsSectionsApart(t *testing.T) {
	o, err := obj.ReadELF(gccObject(t, `
int datavar = 0x11223344;      /* .data     */
int bssvar;                    /* .bss      */
const int rovar = 0x55667788;  /* .rodata   */
extern int peer;
int *ptr = &peer;              /* .data.rel, with a relocation */
int text(void){ return rovar; }
`))
	require.NoError(t, err)

	dv, bv, rv, pv := symOf(t, o, "datavar"), symOf(t, o, "bssvar"), symOf(t, o, "rovar"), symOf(t, o, "ptr")

	// .bss is its own region: a symbol there is not an offset into .data.
	require.Equal(t, obj.SecBss, bv.Section)
	require.GreaterOrEqual(t, o.BssSize, 4, "the .bss symbol needs room reserved for it")

	// The initialized ones share .data and must not overlap.
	for _, s := range []obj.Sym{dv, rv, pv} {
		require.Equal(t, obj.SecData, s.Section, s.Name)
	}
	require.NotEqual(t, dv.Value, rv.Value, "datavar and rovar are different variables")
	require.NotEqual(t, dv.Value, pv.Value, "datavar and ptr are different variables")
	require.NotEqual(t, rv.Value, pv.Value, "rovar and ptr are different variables")

	// And each one's bytes are really there, at its own offset.
	require.GreaterOrEqual(t, len(o.Data), int(pv.Value)+8)
	require.Equal(t, uint32(0x11223344), binary.LittleEndian.Uint32(o.Data[dv.Value:]))
	require.Equal(t, uint32(0x55667788), binary.LittleEndian.Uint32(o.Data[rv.Value:]))

	// ptr's relocation lives in .rela.data.rel -- neither .rela.text nor
	// .rela.data, so reading only those two names dropped it silently.
	found := false
	for _, r := range o.DataRelocs {
		if r.Sym == "peer" {
			require.Equal(t, pv.Value, r.Offset, "the relocation patches ptr's own bytes")
			found = true
		}
	}
	require.True(t, found, "the relocation naming peer must survive: %v", o.DataRelocs)

	// The undefined reference stays undefined.
	require.Equal(t, obj.SecUndef, symOf(t, o, "peer").Section)
	require.NotEmpty(t, o.Text, "the function's code is read")
}

// A section the model cannot place must be named, not quietly filed under .data
// at an offset that means nothing. Silence here is a wrong address.
func TestReadRefusesWhatItCannotPlace(t *testing.T) {
	// A weak symbol: legal to define in several objects, and overridable. The
	// model has neither concept, and calling it strong invents a duplicate-symbol
	// error or binds a call to the wrong definition.
	_, err := obj.ReadELF(gccObject(t, `
__attribute__((weak)) int weakvar = 1;
int use(void){ return weakvar; }
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "weak")
}

// -ffunction-sections gives every function its own .text.NAME. Those are still
// text, and their relocations still matter.
func TestReadFunctionSections(t *testing.T) {
	gcc := testenv.Tool(t, "gcc")
	dir := t.TempDir()
	c := filepath.Join(dir, "f.c")
	obn := filepath.Join(dir, "f.o")
	require.NoError(t, os.WriteFile(c, []byte(`
extern int peer(void);
int a(void){ return peer(); }
int b(void){ return a() + 1; }
`), 0o644))
	out, err := exec.Command(gcc, "-w", "-ffunction-sections", "-c", "-o", obn, c).CombinedOutput()
	require.NoErrorf(t, err, "gcc: %s", out)
	data, err := os.ReadFile(obn)
	require.NoError(t, err)

	o, err := obj.ReadELF(data)
	require.NoError(t, err)

	av, bv := symOf(t, o, "a"), symOf(t, o, "b")
	require.Equal(t, obj.SecText, av.Section)
	require.Equal(t, obj.SecText, bv.Section)
	require.NotEqual(t, av.Value, bv.Value, "two functions in two sections are not at one address")

	// Each .text.NAME has its own .rela.text.NAME; both calls must survive.
	names := map[string]bool{}
	for _, r := range o.Relocs {
		names[r.Sym] = true
		require.Less(t, r.Offset, uint64(len(o.Text)), "the relocation is rebased into the merged text")
	}
	require.True(t, names["peer"], "a's call to peer survives: %v", o.Relocs)
	require.True(t, names["a"], "b's call to a survives: %v", o.Relocs)
}
