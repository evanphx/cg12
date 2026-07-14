package bpf

import (
	"testing"

	"github.com/evanphx/cg12/bpf/internal/libbpfload"
	"github.com/stretchr/testify/require"
)

// TestLibbpfLoad emits an eBPF ELF and loads it through the system libbpf,
// exercising the whole pipeline — BTF-defined maps, .rodata, relocations —
// against the real loader and verifier. Skipped when libbpf is unavailable or
// the caller lacks the privilege to load.
func TestLibbpfLoad(t *testing.T) {
	if !libbpfload.Available() {
		t.Skip("libbpf loader not built (needs cgo on linux)")
	}
	cases := map[string]string{
		"map_and_rodata": elfMapSrc,
		"array_map": `
struct bpf_map_def { unsigned type, key_size, value_size, max_entries, flags; };
struct bpf_map_def counts __attribute__((section("maps"))) = { 2, 4, 8, 64, 0 };
void *bpf_map_lookup_elem(void *m, void *k);
char _license[] __attribute__((section("license"))) = "GPL";
__attribute__((section("xdp"))) int count(void *ctx) {
    unsigned k = 0; unsigned long *v = bpf_map_lookup_elem(&counts, &k);
    if (v) *v += 1;
    return 2;
}`,
		"hash_map_kprobe": `
struct bpf_map_def { unsigned type, key_size, value_size, max_entries, flags; };
struct bpf_map_def events __attribute__((section("maps"))) = { 1, 8, 16, 1024, 0 };
void *bpf_map_lookup_elem(void *m, void *k);
char _license[] __attribute__((section("license"))) = "GPL";
__attribute__((section("kprobe/do_nanosleep"))) int trace(void *ctx) {
    unsigned long key = 42; unsigned long *v = bpf_map_lookup_elem(&events, &key);
    if (v) *v += 1;
    return 0;
}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			obj := compileModule(t, src)
			switch r := libbpfload.Load(obj.ELF()); {
			case r <= -1000:
				t.Skip("libbpf not available at runtime")
			case r == -1 || r == -13: // -EPERM / -EACCES
				t.Skip("no privilege to load eBPF (run as root or with CAP_BPF)")
			case r != 0:
				t.Fatalf("libbpf load/verify failed: errno %d", -r)
			default:
				require.Equal(t, 0, r)
			}
		})
	}
}
