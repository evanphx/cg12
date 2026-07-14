//go:build linux

package bpf

import (
	"encoding/binary"
	"errors"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// skipUnprivileged skips the test when the kernel refuses to load eBPF from this
// process (no CAP_BPF / not root, or the syscall is unavailable).
func skipUnprivileged(t *testing.T, err error) {
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skip("no privilege to load eBPF (run as root or with CAP_BPF)")
	}
	if errors.Is(err, syscall.ENOSYS) {
		t.Skip("bpf() syscall unavailable on this platform")
	}
}

// TestKernelVerifier compiles small C programs and loads their bytecode so the
// in-kernel verifier judges the generated code. Skipped without privileges.
func TestKernelVerifier(t *testing.T) {
	const (
		typeSocketFilter = 1
		typeKprobe       = 2
	)
	cases := []struct {
		name, src string
		progType  uint32
	}{
		{"arith", `int f(void*ctx){ int a=7,b=3; return a*b + (a>b?a:b); }`, typeSocketFilter},
		{"loop", `int f(void*ctx){ int r=1; for(int i=2;i<=6;i++) r*=i; return r; }`, typeSocketFilter},
		{"bitops", `unsigned f(void*ctx){ unsigned x=0xdeadbeef; return (x>>16)^(x<<4)&0xff00; }`, typeSocketFilter},
		{"helper", `unsigned long bpf_ktime_get_ns(void);
		            int f(void*ctx){ return bpf_ktime_get_ns() % 1000; }`, typeKprobe},
		{"pid", `unsigned long bpf_get_current_pid_tgid(void);
		         int f(void*ctx){ unsigned long p=bpf_get_current_pid_tgid()>>32; return p>1000?1:0; }`, typeKprobe},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := compileC(t, c.src)
			p, err := Compile(f)
			require.NoError(t, err)
			fd, log, err := LoadProgram(c.progType, p.Bytes(), "GPL")
			if err != nil {
				skipUnprivileged(t, err)
				t.Fatalf("verifier rejected the program:\n%s", log)
			}
			syscall.Close(fd)
		})
	}
}

// TestKernelMapProgram compiles a real stateful program — an XDP packet counter
// backed by an array map — loads it (creating the map and patching its fd),
// runs it, and reads the counter back. Skipped without privileges.
func TestKernelMapProgram(t *testing.T) {
	const src = `
struct bpf_map_def { unsigned type, key_size, value_size, max_entries, flags; };
struct bpf_map_def counts __attribute__((section("maps"))) = { 2 /*ARRAY*/, 4, 8, 4, 0 };
void *bpf_map_lookup_elem(void *map, void *key);
__attribute__((section("xdp"))) int count(void *ctx) {
    unsigned key = 0;
    unsigned long *v = bpf_map_lookup_elem(&counts, &key);
    if (v) *v += 1;
    return 2; // XDP_PASS
}`
	obj := compileModule(t, src)
	require.Len(t, obj.Maps, 1)
	require.Equal(t, "counts", obj.Maps[0].Name)
	require.Len(t, obj.Programs, 1)
	require.Equal(t, "xdp", obj.Programs[0].Section)
	require.NotEmpty(t, obj.Programs[0].Relocs, "the &counts reference must be a map relocation")

	const typeXDP = 6
	lo, err := Load(obj, typeXDP)
	if err != nil {
		skipUnprivileged(t, err)
		t.Fatalf("load failed: %v", err)
	}
	defer lo.Close()

	ret, err := lo.TestRun(make([]byte, 14), 5) // 5 runs on a minimal ethernet frame
	require.NoError(t, err)
	require.Equal(t, uint32(2), ret, "XDP_PASS")

	key := make([]byte, 4)
	val := make([]byte, 8)
	require.NoError(t, MapLookup(lo.Maps["counts"], key, val))
	require.Equal(t, uint64(5), binary.LittleEndian.Uint64(val), "counter incremented once per run")
}

// TestKernelRodataPrintk loads a program that reads a format string from .rodata
// (a frozen array map) and calls bpf_trace_printk — exercising the whole
// string-constant path against the verifier. Skipped without privileges.
func TestKernelRodataPrintk(t *testing.T) {
	const src = `
long bpf_trace_printk(const char *fmt, unsigned sz, ...);
char _license[] __attribute__((section("license"))) = "GPL";
__attribute__((section("xdp"))) int say(void *ctx) {
    bpf_trace_printk("cg12 %d\n", 9, 42);
    return 2;
}`
	obj := compileModule(t, src)
	require.Equal(t, "GPL", obj.License)
	// The .rodata map (holding the format string) must be present.
	rodata, ok := obj.MapByName(".rodata")
	require.True(t, ok, "the format string must live in a .rodata map")
	require.NotEmpty(t, rodata.Initial)

	lo, err := Load(obj, 6 /*XDP*/)
	if err != nil {
		skipUnprivileged(t, err)
		t.Fatalf("load failed: %v", err)
	}
	defer lo.Close()
	ret, err := lo.TestRun(make([]byte, 14), 1)
	require.NoError(t, err)
	require.Equal(t, uint32(2), ret)
}

// TestKernelDataGlobal exercises a mutable global (.data): a program that adds a
// const step (.rodata) to a running total (.data) must accumulate across runs.
func TestKernelDataGlobal(t *testing.T) {
	const src = `
int total = 0;
const int step = 5;
char _license[] __attribute__((section("license"))) = "GPL";
__attribute__((section("xdp"))) int prog(void *ctx) { total += step; return total; }`
	obj := compileModule(t, src)
	_, hasRodata := obj.MapByName(".rodata")
	_, hasData := obj.MapByName(".data")
	require.True(t, hasRodata, "const step in .rodata")
	require.True(t, hasData, "mutable total in .data")

	lo, err := Load(obj, 6 /*XDP*/)
	if err != nil {
		skipUnprivileged(t, err)
		t.Fatalf("load failed: %v", err)
	}
	defer lo.Close()
	ret, err := lo.TestRun(make([]byte, 14), 3)
	require.NoError(t, err)
	require.Equal(t, uint32(15), ret, "3 runs * step 5")

	val := make([]byte, 4)
	require.NoError(t, MapLookup(lo.Maps[".data"], make([]byte, 4), val))
	require.Equal(t, uint32(15), binary.LittleEndian.Uint32(val), ".data total persisted across runs")
}

// TestKernelPerCPUMap exercises a BPF_MAP_TYPE_PERCPU_ARRAY (type 6): the map
// type is just a number in the definition, so the whole map machinery must work
// unchanged. A lookup returns this CPU's slot, which the program bumps.
func TestKernelPerCPUMap(t *testing.T) {
	const src = `
struct bpf_map_def { unsigned type, key_size, value_size, max_entries, flags; };
struct bpf_map_def pc __attribute__((section("maps"))) = { 6 /*PERCPU_ARRAY*/, 4, 8, 1, 0 };
void *bpf_map_lookup_elem(void *map, void *key);
char _license[] __attribute__((section("license"))) = "GPL";
__attribute__((section("xdp"))) int prog(void *ctx) {
    unsigned k = 0;
    unsigned long *v = bpf_map_lookup_elem(&pc, &k);
    if (v) { *v += 3; return (int)*v; }
    return 0;
}`
	obj := compileModule(t, src)
	require.Len(t, obj.Maps, 1)
	require.Equal(t, uint32(6), obj.Maps[0].Type, "PERCPU_ARRAY")

	lo, err := Load(obj, 6 /*XDP*/)
	if err != nil {
		skipUnprivileged(t, err)
		t.Fatalf("load failed: %v", err)
	}
	defer lo.Close()
	ret, err := lo.TestRun(make([]byte, 14), 1)
	require.NoError(t, err)
	require.Equal(t, uint32(3), ret)
}

// TestKernelTailCall wires a real tail call: an entry program jumps through a
// PROG_ARRAY (type 3) into a target program that returns 42. The loader loads
// both programs, stores the target's fd at index 0 of the array, and the tail
// call transfers control — proving multi-program objects and bpf_tail_call work.
func TestKernelTailCall(t *testing.T) {
	const src = `
struct bpf_map_def { unsigned type, key_size, value_size, max_entries, flags; };
struct bpf_map_def jmp __attribute__((section("maps"))) = { 3 /*PROG_ARRAY*/, 4, 4, 4, 0 };
long bpf_tail_call(void *ctx, void *map, unsigned index);
char _license[] __attribute__((section("license"))) = "GPL";
__attribute__((section("xdp"))) int target(void *ctx) { return 42; }
__attribute__((section("xdp"))) int entry(void *ctx) { bpf_tail_call(ctx, &jmp, 0); return 1; }`
	obj := compileModule(t, src)
	require.Len(t, obj.Programs, 2)

	lo, err := Load(obj, 6 /*XDP*/)
	if err != nil {
		skipUnprivileged(t, err)
		t.Fatalf("load failed: %v", err)
	}
	defer lo.Close()

	// Install the target program at index 0 of the jump table.
	val := make([]byte, 4)
	binary.LittleEndian.PutUint32(val, uint32(lo.Progs["target"]))
	require.NoError(t, MapUpdate(lo.Maps["jmp"], make([]byte, 4), val, 0))

	ret, err := lo.TestRunProg("entry", make([]byte, 14), 1)
	require.NoError(t, err)
	require.Equal(t, uint32(42), ret, "the tail call transferred control to target")
}

// TestKernelSubprograms compiles a multi-function program (unoptimized, so the
// BPF-to-BPF calls survive), links it, and runs it — checking the call chain
// produces the right result. Skipped without privileges.
func TestKernelSubprograms(t *testing.T) {
	const src = `
static int square(int x) { return x * x; }
static int sum_squares(int a, int b) { return square(a) + square(b); }
char _license[] __attribute__((section("license"))) = "GPL";
__attribute__((section("xdp"))) int prog(void *ctx) { return sum_squares(3, 4); }`
	// Compile without the optimizer so the subprogram calls are not inlined away.
	m, err := ccCompile(t, src)
	require.NoError(t, err)
	obj, err := CompileModule(m)
	require.NoError(t, err)
	require.Len(t, obj.Programs, 1)
	require.Greater(t, len(obj.Programs[0].Insns), 20, "entry + two subprograms concatenated")

	lo, err := Load(obj, 6 /*XDP*/)
	if err != nil {
		skipUnprivileged(t, err)
		t.Fatalf("load failed: %v", err)
	}
	defer lo.Close()
	ret, err := lo.TestRun(make([]byte, 14), 1)
	require.NoError(t, err)
	require.Equal(t, uint32(25), ret, "sum_squares(3,4) = 3*3 + 4*4")
}
