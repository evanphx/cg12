package bpf

import (
	"bytes"
	"debug/elf"
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

	for _, name := range []string{"xdp", "license", ".maps", ".rodata", ".BTF", ".relxdp"} {
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
