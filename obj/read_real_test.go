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
func gccObject(t *testing.T, src string, flags ...string) []byte {
	t.Helper()
	gcc := testenv.Tool(t, "gcc")
	dir := t.TempDir()
	c := filepath.Join(dir, "x.c")
	o := filepath.Join(dir, "x.o")
	require.NoError(t, os.WriteFile(c, []byte(src), 0o644))
	argv := append([]string{"-w", "-c", "-o", o}, flags...)
	out, err := exec.Command(gcc, append(argv, c)...).CombinedOutput()
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

	// The const one is read-only data: its own region, because that is where the
	// promise is kept.
	require.Equal(t, obj.SecRodata, rv.Section)
	require.GreaterOrEqual(t, len(o.Rodata), int(rv.Value)+4)
	require.Equal(t, uint32(0x55667788), binary.LittleEndian.Uint32(o.Rodata[rv.Value:]))

	// The writable ones share .data and must not overlap.
	for _, s := range []obj.Sym{dv, pv} {
		require.Equal(t, obj.SecData, s.Section, s.Name)
	}
	require.NotEqual(t, dv.Value, pv.Value, "datavar and ptr are different variables")
	require.GreaterOrEqual(t, len(o.Data), int(pv.Value)+8)
	require.Equal(t, uint32(0x11223344), binary.LittleEndian.Uint32(o.Data[dv.Value:]))

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

// gcc puts `static const char *const []` in .data.rel.ro.local: const data that
// still needs relocating. Our model has a section for exactly that, and reading
// the object must find it there.
//
// The name starts with ".data", so the obvious prefix match files it as ordinary
// writable data. Nothing then reads the wrong bytes -- which is why this went
// unnoticed -- but the data stays writable for the life of the process, losing
// the guarantee the section exists to make.
func TestReadRealObjectFindsDataRelRo(t *testing.T) {
	data := gccObject(t, `
		static const char *const words[] = {"alpha", "beta"};
		const char *pick(int i) { return words[i]; }
	`, "-fPIC", "-O2")
	o, err := obj.ReadELF(data)
	require.NoError(t, err)

	require.NotEmpty(t, o.Relro, "the pointer table is .data.rel.ro, not .data")
	require.Len(t, o.Relro, 16, "two pointers")
	require.Len(t, o.RelroRelocs, 2,
		"one relocation per pointer, filed against .data.rel.ro rather than .data")

	// The strings themselves hold no address, so they stay in real read-only data.
	require.NotEmpty(t, o.Rodata, "the string bodies are .rodata")
	require.Empty(t, o.RodataRelocs, "and nothing there needs relocating")

	// Not checked here: what those two relocations point AT. gcc files them
	// against the section symbol .rodata.str1.8, and ReadELF drops section symbols,
	// so both come back with an empty target name. That is #134 -- it predates this
	// section and hits any object with a string literal in it -- and asserting the
	// wrong thing here would make this test fail for that reason instead of its own.
}
