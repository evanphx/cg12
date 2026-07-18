package arm64_test

import (
	"bytes"
	dwarfelf "debug/elf"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runObject links a pre-compiled object with a C driver, runs it, and returns
// the exit code.
func runObject(t *testing.T, data []byte, cmain string) int {
	t.Helper()
	cc := assembler(t)
	dir := t.TempDir()
	objPath := filepath.Join(dir, "u.o")
	cPath := filepath.Join(dir, "main.c")
	bin := filepath.Join(dir, "prog")
	require.NoError(t, os.WriteFile(objPath, data, 0o644))
	require.NoError(t, os.WriteFile(cPath, []byte(cmain), 0o644))
	out, err := exec.Command(cc, "-o", bin, objPath, cPath).CombinedOutput()
	require.NoErrorf(t, err, "link failed: %s", out)
	if err := exec.Command(bin).Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		t.Fatalf("run: %v", err)
	}
	return 0
}

// TestGCStrategyEmitsPoll shows a GC strategy lowering a safepoint into a real
// cooperative poll — late, at emit time. The IR only marks b.Safepoint(); the
// PollStrategy emits the flag check and runtime call. At run time the poll
// actually fires the runtime handler.
func TestGCStrategyEmitsPoll(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("spin", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	done := f.NewBlock("done")
	start.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), done, body)
	body.Safepoint() // just a marker; the strategy turns it into a poll
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, i2)
	done.Ret(i)

	opts := arm64.Options{GC: arm64.PollStrategy{FlagSym: "gc_poll_flag", StubSym: "gc_safepoint"}}
	data, err := arm64.CompileObjectWith(m, opts)
	require.NoError(t, err)

	// The strategy's runtime call appears as a relocation the normal (GC-less)
	// build does not have.
	assert.True(t, hasTextReloc(t, data, "gc_safepoint"), "poll calls the runtime stub")
	plain, err := arm64.CompileObject(m)
	require.NoError(t, err)
	assert.False(t, hasTextReloc(t, plain, "gc_safepoint"), "no GC code without a strategy")

	code := runObject(t, data, `
extern int spin(int);
unsigned char gc_poll_flag = 1; // the runtime requests a collection
int g_polls = 0;
void gc_safepoint(void){ g_polls++; gc_poll_flag = 0; } // fire once, then clear
int main(void){ spin(100); return g_polls == 1 ? 0 : 1; }`)
	require.Equal(t, 0, code, "the poll fired the runtime exactly once")
}

// hasTextReloc reports whether the object has a .text relocation naming sym.
func hasTextReloc(t *testing.T, data []byte, sym string) bool {
	t.Helper()
	ef, err := dwarfelf.NewFile(bytes.NewReader(data))
	require.NoError(t, err)
	syms, err := ef.Symbols()
	require.NoError(t, err)
	rela := ef.Section(".rela.text")
	if rela == nil {
		return false
	}
	b, err := rela.Data()
	require.NoError(t, err)
	for off := 0; off+24 <= len(b); off += 24 {
		symIdx := binary.LittleEndian.Uint64(b[off+8:]) >> 32
		if int(symIdx) >= 1 && int(symIdx)-1 < len(syms) && syms[symIdx-1].Name == sym {
			return true
		}
	}
	return false
}

type smRoot struct {
	kind byte
	val  int32
	typ  uint32
}

// parseStackMap decodes the .cg12_stackmaps section into per-safepoint roots.
func parseStackMap(t *testing.T, data []byte) [][]smRoot {
	t.Helper()
	ef, err := dwarfelf.NewFile(bytes.NewReader(data))
	require.NoError(t, err)
	sec := ef.Section(".cg12_stackmaps")
	require.NotNil(t, sec, "has .cg12_stackmaps")
	b, err := sec.Data()
	require.NoError(t, err)

	require.Equal(t, "SMAP", string(b[0:4]))
	assert.Equal(t, uint16(2), binary.LittleEndian.Uint16(b[4:]))
	count := binary.LittleEndian.Uint32(b[8:])
	p := 12
	var out [][]smRoot
	for i := uint32(0); i < count; i++ {
		p += 8 // pc (relocated; zero in the object)
		nroots := binary.LittleEndian.Uint32(b[p:])
		p += 4
		var roots []smRoot
		for j := uint32(0); j < nroots; j++ {
			roots = append(roots, smRoot{
				kind: b[p],
				val:  int32(binary.LittleEndian.Uint32(b[p+4:])),
				typ:  binary.LittleEndian.Uint32(b[p+8:]),
			})
			p += 12
		}
		out = append(out, roots)
	}

	// The section is discoverable through a symbol.
	syms, _ := ef.Symbols()
	found := false
	for _, s := range syms {
		if s.Name == "__cg12_stackmaps" {
			found = true
		}
	}
	assert.True(t, found, "__cg12_stackmaps symbol present")
	return out
}

// A managed reference live across a call is reported as a root at that safepoint,
// in the register the allocator placed it (a callee-saved register, since it
// survives the call).
func TestObjEmitStackMapRootAcrossCall(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("scan", ir.ClsL).Export()
	p := f.ParamRef("obj") // GC reference
	n := f.Param("n", ir.ClsW)
	e := f.Entry()
	e.Call(ir.ClsW, f.Sym("sink", 0), n) // obj is live across this call
	e.Ret(p)

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)

	points := parseStackMap(t, data)
	require.Len(t, points, 1, "one safepoint")
	require.Len(t, points[0], 1, "one live root across the call")
	// A reference live across a safepoint is spilled, so the root is a frame slot.
	assert.Equal(t, byte(1), points[0][0].kind, "root is a frame slot")
	assert.GreaterOrEqual(t, points[0][0].val, int32(0), "frame-pointer offset")
}

func TestObjEmitStackMapKeepsAggregatePointerResultAcrossCall(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("aggregateResultRoot", ir.ClsP).Export()
	f.GoABI = true
	sliceType := &ir.AggType{
		Name:  "slice",
		Align: 8,
		Fields: []ir.Field{
			{Sub: ir.SubL, Pointer: true},
			{Sub: ir.SubL},
			{Sub: ir.SubL},
		},
	}
	block := f.Entry()
	parts := block.CallAggregate(
		sliceType,
		[]ir.Cls{ir.ClsP, ir.ClsL, ir.ClsL},
		f.Sym("returnSlice", 0),
	)
	block.CallVoid(f.Sym("collect", 0))
	block.Ret(parts[0])

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)

	points := parseStackMap(t, data)
	foundRoot := false
	for _, roots := range points {
		if len(roots) != 0 {
			foundRoot = true
		}
	}
	assert.True(t, foundRoot, "the aggregate pointer result is live across collect")
}

// An explicit safepoint that is NOT a call still emits a stack map for the
// references live there — the inlined-allocation / loop-poll case.
func TestObjEmitStackMapExplicitSafepoint(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("poll", ir.ClsL).Export()
	p := f.ParamRef("obj")
	e := f.Entry()
	e.Safepoint() // not a call; obj is live across it (returned below)
	e.Ret(p)

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)

	// The IR has no call, yet a safepoint with a root is emitted — it can only
	// come from the explicit marker.
	points := parseStackMap(t, data)
	require.Len(t, points, 1, "the explicit safepoint")
	require.Len(t, points[0], 1, "obj is a live root")

	// It links and runs (the marker emits no code, returns its argument).
	_, code := buildAndRun(t, m, `
extern void *poll(void*);
int main(void){ int v; return poll(&v)==&v ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// A safepoint on a loop back-edge keeps a reference live across an
// otherwise-call-free loop reported as a root.
func TestObjEmitStackMapLoopBackedge(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("spin", ir.ClsL).Export()
	obj := f.ParamRef("obj")
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	done := f.NewBlock("done")
	start.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), done, body)
	body.Safepoint() // poll on the back-edge; obj is live across it
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, i2)
	done.Ret(obj)

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	points := parseStackMap(t, data)
	require.Len(t, points, 1)
	require.Len(t, points[0], 1, "obj is live across the loop poll")

	_, code := buildAndRun(t, m, `
extern void *spin(void*, int);
int main(void){ int v; return spin(&v, 5)==&v ? 0 : 1; }`)
	require.Equal(t, 0, code)
}

// TestObjEmitStackMapWalkedByCollector is the end-to-end proof: a real "garbage
// collector" (the C gc_collect below), triggered at a safepoint, walks the
// frame-pointer chain, looks up the safepoint's return address in
// .cg12_stackmaps, reads the root from the caller's frame, and recovers the
// exact live pointer — using nothing but the data cg12 emitted.
func TestObjEmitStackMapWalkedByCollector(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("scan", ir.ClsL).Export()
	obj := f.ParamRef("obj") // a managed reference, held live across the call
	e := f.Entry()
	e.CallVoid(f.Sym("gc_collect", 0)) // the collection safepoint
	e.Ret(obj)

	_, code := buildAndRun(t, m, `
#include <stdint.h>
#include <string.h>

extern unsigned char __cg12_stackmaps[]; // the emitted stack-map table

static void    *g_root;
static uint32_t g_nroots = 0xffffffff;

// A minimal precise root scanner: find the stack-map entry for our return
// address and read each frame-slot root out of the caller's frame.
void gc_collect(void) {
	void *my_fp     = __builtin_frame_address(0);
	void *ret       = __builtin_return_address(0); // safepoint PC in the caller
	void *caller_fp = *(void **)my_fp;             // caller's saved x29

	unsigned char *p = __cg12_stackmaps;
	if (memcmp(p, "SMAP", 4) != 0) return;
	uint32_t count;
	memcpy(&count, p + 8, 4);
	p += 12;
	for (uint32_t i = 0; i < count; i++) {
		uint64_t pc;
		uint32_t nroots;
		memcpy(&pc, p, 8);
		memcpy(&nroots, p + 8, 4);
		p += 12;
		if ((void *)(uintptr_t)pc == ret) {
			g_nroots = nroots;
			for (uint32_t j = 0; j < nroots; j++) {
				uint8_t kind = p[0];
				int32_t off;
				memcpy(&off, p + 4, 4);
				p += 8;
				if (kind == 1) // frame slot: read the root from the caller's frame
					g_root = *(void **)((char *)caller_fp + off);
			}
			return;
		}
		p += (size_t)nroots * 8;
	}
}

extern void *scan(void *);
int main(void) {
	void *obj = (void *)0x123456780ULL;
	void *r   = scan(obj);
	if (r != obj)      return 1; // sanity: scan returns its argument
	if (g_nroots != 1) return 2; // the collector found exactly one root
	if (g_root != obj) return 3; // and it is the live object, recovered precisely
	return 0;
}`)
	require.Equal(t, 0, code)
}

// A typed managed reference carries its type descriptor into the stack map.
func TestObjEmitStackMapCarriesType(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("scan3", ir.ClsL).Export()
	p := f.ParamRef("obj")
	f.MarkGCRefType(p, 0xABCD) // tag the reference with a runtime type id
	n := f.Param("n", ir.ClsW)
	e := f.Entry()
	e.Call(ir.ClsW, f.Sym("sink", 0), n)
	e.Ret(p)

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	points := parseStackMap(t, data)
	require.Len(t, points, 1)
	require.Len(t, points[0], 1)
	assert.Equal(t, uint32(0xABCD), points[0][0].typ, "root carries its type")
}

// A pointer that is NOT marked as a managed reference is not reported as a root,
// even when live across the call.
func TestObjEmitStackMapExcludesNonRefs(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("scan2", ir.ClsL).Export()
	raw := f.Param("raw", ir.ClsL) // a raw long/pointer, not a GC ref
	n := f.Param("n", ir.ClsW)
	e := f.Entry()
	e.Call(ir.ClsW, f.Sym("sink", 0), n)
	e.Ret(raw)

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)

	// No safepoint carries a root, so the stack-map section is omitted entirely.
	ef, err := dwarfelf.NewFile(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Nil(t, ef.Section(".cg12_stackmaps"), "no roots -> no stack-map section")
}

// A spilled GC reference is reported as a frame-relative root, and the whole
// thing still links and runs.
func TestObjEmitStackMapSpilledAndRuns(t *testing.T) {
	m := ir.NewModule()
	// helper the driver provides: consumes an int.
	f := m.NewFunc("gckeep", ir.ClsL).Export()
	p := f.ParamRef("obj")
	n := f.Param("n", ir.ClsW)
	e := f.Entry()
	// Two calls keep obj live across both; return obj unchanged.
	e.Call(ir.ClsW, f.Sym("sink", 0), n)
	e.Call(ir.ClsW, f.Sym("sink", 0), n)
	e.Ret(p)

	data, err := arm64.CompileObject(m)
	require.NoError(t, err)
	points := parseStackMap(t, data)
	require.Len(t, points, 2, "two safepoints")
	for _, roots := range points {
		require.Len(t, roots, 1)
	}

	// It links and runs: gckeep returns its pointer argument untouched.
	_, code := buildAndRun(t, m, `
#include <stdint.h>
int sink(int x){ return x; }
extern void *gckeep(void*, int);
int main(void){ int v=42; return gckeep(&v, 7)==&v ? 0 : 1; }`)
	require.Equal(t, 0, code)
}
