package obj_test

import (
	"bytes"
	dwarfelf "debug/elf"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func words(ws ...uint32) []byte {
	b := make([]byte, 4*len(ws))
	for i, w := range ws {
		binary.LittleEndian.PutUint32(b[4*i:], w)
	}
	return b
}

// linkRun writes the object, links it with a C driver, and runs the result,
// returning the exit code. It skips when no AArch64 toolchain is present.
func linkRun(t *testing.T, objBytes []byte, cmain string) int {
	t.Helper()
	cc, err := exec.LookPath("aarch64-linux-gnu-gcc")
	if err != nil {
		t.Skip("no AArch64 toolchain available")
	}
	dir := t.TempDir()
	objPath := filepath.Join(dir, "unit.o")
	cPath := filepath.Join(dir, "main.c")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(objPath, objBytes, 0o644))
	require.NoError(t, os.WriteFile(cPath, []byte(cmain), 0o644))

	out, err := exec.Command(cc, "-o", bin, objPath, cPath).CombinedOutput()
	require.NoErrorf(t, err, "link failed: %s", out)
	err = exec.Command(bin).Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	require.NoError(t, err)
	return 0
}

// addfnObject is add(a, b) = a + b as an AArch64 object.
func addfnObject() *obj.Object {
	text := words(a64.AddReg(false, 0, 0, 1), a64.Ret(30))
	return &obj.Object{
		Machine: obj.EM_AARCH64,
		Text:    text,
		Syms:    []obj.Sym{{Name: "addfn", Section: obj.SecText, Size: uint64(len(text)), Global: true, Func: true}},
	}
}

func TestObjectDefinedSymbolLinksAndRuns(t *testing.T) {
	data, err := addfnObject().MarshalELF()
	require.NoError(t, err)
	code := linkRun(t, data, `
extern int addfn(int, int);
int main(void){ return addfn(3, 4) == 7 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestObjectRelocationLinksAndRuns(t *testing.T) {
	// caller() = helper() + 1, calling an external helper via a CALL26 relocation.
	text := words(
		a64.SubImm(true, 31, 31, 16), // sub sp, sp, #16
		a64.StrImm(true, 30, 31, 8),  // str x30, [sp, #8]
		a64.Bl(0),                    // bl helper   (offset 8, patched by the linker)
		a64.AddImm(false, 0, 0, 1),   // add w0, w0, #1
		a64.LdrImm(true, 30, 31, 8),  // ldr x30, [sp, #8]
		a64.AddImm(true, 31, 31, 16), // add sp, sp, #16
		a64.Ret(30),
	)
	o := &obj.Object{
		Machine: obj.EM_AARCH64,
		Text:    text,
		Syms:    []obj.Sym{{Name: "caller", Section: obj.SecText, Size: uint64(len(text)), Global: true, Func: true}},
		Relocs:  []obj.Reloc{{Offset: 8, Sym: "helper", Type: obj.R_AARCH64_CALL26}},
	}
	data, err := o.MarshalELF()
	require.NoError(t, err)
	code := linkRun(t, data, `
extern int caller(void);
int helper(void){ return 41; }
int main(void){ return caller() == 42 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

// TestObjectParsesWithDebugElf validates the ELF structure independently with
// Go's read-only ELF parser — no external tools.
func TestObjectParsesWithDebugElf(t *testing.T) {
	text := words(a64.SubImm(true, 31, 31, 16), a64.Bl(0), a64.Ret(30))
	o := &obj.Object{
		Machine: obj.EM_AARCH64,
		Text:    text,
		Syms:    []obj.Sym{{Name: "f", Section: obj.SecText, Size: uint64(len(text)), Global: true, Func: true}},
		Relocs:  []obj.Reloc{{Offset: 4, Sym: "ext", Type: obj.R_AARCH64_CALL26}},
	}
	data, err := o.MarshalELF()
	require.NoError(t, err)

	f, err := dwarfelf.NewFile(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, dwarfelf.EM_AARCH64, f.Machine)
	assert.Equal(t, dwarfelf.ET_REL, f.Type)

	require.NotNil(t, f.Section(".text"))
	td, _ := f.Section(".text").Data()
	assert.Equal(t, text, td)

	syms, err := f.Symbols()
	require.NoError(t, err)
	byName := map[string]dwarfelf.Symbol{}
	for _, s := range syms {
		byName[s.Name] = s
	}
	require.Contains(t, byName, "f")
	assert.Equal(t, dwarfelf.STT_FUNC, dwarfelf.ST_TYPE(byName["f"].Info))
	assert.Equal(t, dwarfelf.STB_GLOBAL, dwarfelf.ST_BIND(byName["f"].Info))
	require.Contains(t, byName, "ext")
	assert.Equal(t, dwarfelf.SHN_UNDEF, byName["ext"].Section, "external symbol is undefined")

	// One relocation of type CALL26 at offset 4.
	rela := f.Section(".rela.text")
	require.NotNil(t, rela)
	rd, _ := rela.Data()
	require.Len(t, rd, 24)
	assert.Equal(t, uint64(4), binary.LittleEndian.Uint64(rd[0:]))                            // r_offset
	assert.Equal(t, uint32(obj.R_AARCH64_CALL26), uint32(binary.LittleEndian.Uint64(rd[8:]))) // r_info type
}

func TestObjectDataRelocationParsesAndLinks(t *testing.T) {
	// .data holds a word table and an 8-byte pointer slot; a .rela.data ABS64
	// relocation makes the pointer point at table+8 (the third word).
	data := make([]byte, 24) // [0:16] = four words, [16:24] = pointer slot
	binary.LittleEndian.PutUint32(data[0:], 10)
	binary.LittleEndian.PutUint32(data[4:], 20)
	binary.LittleEndian.PutUint32(data[8:], 30)
	binary.LittleEndian.PutUint32(data[12:], 40)
	o := &obj.Object{
		Machine: obj.EM_AARCH64,
		Data:    data,
		Syms: []obj.Sym{
			{Name: "table", Section: obj.SecData, Value: 0, Size: 16, Global: true},
			{Name: "pptr", Section: obj.SecData, Value: 16, Size: 8, Global: true},
		},
		DataRelocs: []obj.Reloc{{Offset: 16, Sym: "table", Type: obj.R_AARCH64_ABS64, Addend: 8}},
	}
	bin, err := o.MarshalELF()
	require.NoError(t, err)

	// debug/elf sees a .rela.data section carrying the ABS64 relocation.
	f, err := dwarfelf.NewFile(bytes.NewReader(bin))
	require.NoError(t, err)
	rela := f.Section(".rela.data")
	require.NotNil(t, rela)
	rd, _ := rela.Data()
	require.Len(t, rd, 24)
	assert.Equal(t, uint64(16), binary.LittleEndian.Uint64(rd[0:]))                          // r_offset
	assert.Equal(t, uint32(obj.R_AARCH64_ABS64), uint32(binary.LittleEndian.Uint64(rd[8:]))) // r_info type
	assert.Equal(t, int64(8), int64(binary.LittleEndian.Uint64(rd[16:])))                    // r_addend

	// Link it and confirm the relocated pointer dereferences to 30.
	code := linkRun(t, bin, `
extern int *pptr;
int main(void){ return *pptr==30 ? 0 : 1; }`)
	assert.Equal(t, 0, code)
}

func TestObjectUnknownRelocSymbolNotAnError(t *testing.T) {
	// A relocation to a symbol not defined here is fine — it becomes an external
	// (undefined) symbol resolved at link time.
	o := &obj.Object{
		Machine: obj.EM_AARCH64,
		Text:    words(a64.Bl(0), a64.Ret(30)),
		Relocs:  []obj.Reloc{{Offset: 0, Sym: "someext", Type: obj.R_AARCH64_JUMP26}},
	}
	_, err := o.MarshalELF()
	require.NoError(t, err)
}
