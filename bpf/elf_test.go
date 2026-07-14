package bpf

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const elfMapSrc = `
struct bpf_map_def { unsigned type, key_size, value_size, max_entries, flags; };
struct bpf_map_def counts __attribute__((section("maps"))) = { 2, 4, 8, 64, 0 };
long bpf_trace_printk(const char *fmt, unsigned sz, ...);
void *bpf_map_lookup_elem(void *m, void *k);
char _license[] __attribute__((section("license"))) = "GPL";
__attribute__((section("xdp"))) int count(void *ctx) {
    unsigned k = 0;
    unsigned long *v = bpf_map_lookup_elem(&counts, &k);
    if (v) { *v += 1; bpf_trace_printk("hi %lu\n", 7, *v); }
    return 2;
}`

// TestELFStructure checks the emitted object is a well-formed eBPF ELF that Go's
// own ELF reader parses: the right machine/type, the expected sections, a BTF
// blob, symbols for the program/map/rodata, and a relocation for the map load.
func TestELFStructure(t *testing.T) {
	obj := compileModule(t, elfMapSrc)
	f, err := elf.NewFile(bytes.NewReader(obj.ELF()))
	require.NoError(t, err)
	defer f.Close()

	assert.Equal(t, elf.EM_BPF, f.Machine)
	assert.Equal(t, elf.ET_REL, f.Type)

	for _, name := range []string{"xdp", "license", ".maps", ".rodata", ".BTF", ".BTF.ext", ".relxdp"} {
		assert.NotNilf(t, f.Section(name), "missing section %s", name)
	}

	// The program section holds whole 8-byte instructions.
	xdp := f.Section("xdp")
	assert.Zero(t, xdp.Size%8)
	assert.Equal(t, elf.SHF_ALLOC|elf.SHF_EXECINSTR, elf.SectionFlag(xdp.Flags))

	// The BTF section starts with the BTF magic (0xeb9f, little-endian).
	btf, err := f.Section(".BTF").Data()
	require.NoError(t, err)
	assert.Equal(t, []byte{0x9f, 0xeb}, btf[:2])

	// Symbols name the program, the map, and the rodata global.
	syms, err := f.Symbols()
	require.NoError(t, err)
	names := map[string]bool{}
	for _, s := range syms {
		names[s.Name] = true
	}
	assert.True(t, names["count"], "program symbol")
	assert.True(t, names["counts"], "map symbol")

	// The map/rodata loads are relocated (R_BPF_64_64 entries exist).
	rels, err := f.Section(".relxdp").Data()
	require.NoError(t, err)
	assert.NotEmpty(t, rels)
	assert.Zero(t, len(rels)%16, "each relocation is 16 bytes")
}

// TestBTFExt decodes the emitted .BTF.ext and checks its func_info names the
// program in its section and its line_info carries valid, byte-addressed,
// strictly-increasing source records — the layout the kernel enforces.
func TestBTFExt(t *testing.T) {
	obj := compileModule(t, elfMapSrc)
	f, err := elf.NewFile(bytes.NewReader(obj.ELF()))
	require.NoError(t, err)
	defer f.Close()

	btf, err := f.Section(".BTF").Data()
	require.NoError(t, err)
	ext, err := f.Section(".BTF.ext").Data()
	require.NoError(t, err)

	le := func(b []byte, o int) uint32 { return binary.LittleEndian.Uint32(b[o:]) }
	// BTF string section: str_off/str_len live at header offsets 16/20.
	strs := btf[24+le(btf, 12):]
	str := func(off uint32) string {
		end := off
		for int(end) < len(strs) && strs[end] != 0 {
			end++
		}
		return string(strs[off:end])
	}

	assert.Equal(t, uint16(0xeb9f), binary.LittleEndian.Uint16(ext))
	hdr := int(le(ext, 4))
	funcOff, funcLen := hdr+int(le(ext, 8)), int(le(ext, 12))
	lineOff := hdr + int(le(ext, 16))

	// func_info: rec_size 8, then a (sec_name, count, records) group. The program
	// "count" must appear at instruction offset 0 of the "xdp" section.
	assert.Equal(t, uint32(8), le(ext, funcOff), "bpf_func_info rec_size")
	assert.Equal(t, "xdp", str(le(ext, funcOff+4)))
	require.Equal(t, uint32(1), le(ext, funcOff+8), "one function in xdp")
	assert.Zero(t, le(ext, funcOff+12), "func at byte offset 0")
	funcTypeID := le(ext, funcOff+16)
	assert.NotZero(t, funcTypeID)

	// line_info: rec_size 16, then a group for "xdp". Records must be byte-based
	// (multiples of 8), strictly increasing, and carry a real file and line.
	assert.Equal(t, uint32(16), le(ext, lineOff), "bpf_line_info rec_size")
	assert.Equal(t, "xdp", str(le(ext, lineOff+4)))
	n := int(le(ext, lineOff+8))
	require.Greater(t, n, 1, "several source lines")
	prev := -1
	for i := 0; i < n; i++ {
		r := lineOff + 12 + i*16
		insnOff := int(le(ext, r))
		assert.Zero(t, insnOff%8, "insn offsets are in bytes")
		assert.Greater(t, insnOff, prev, "strictly increasing")
		prev = insnOff
		assert.Equal(t, "prog.c", str(le(ext, r+4)), "records name the source file")
		assert.NotZero(t, le(ext, r+12)>>10, "line number present")
	}
	assert.Less(t, funcOff+funcLen, lineOff+16, "func_info precedes line_info")
}
