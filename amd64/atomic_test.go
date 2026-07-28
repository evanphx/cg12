package amd64_test

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/cc"
	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/require"
)

// Atomics are tested in two layers, and the split is not incidental: neither layer
// can do the other's job.
//
// The execution layer below runs real instructions on this host and checks the
// values they produce -- old versus new, success versus failure, every width, the
// sign and wraparound edges. What it cannot check is atomicity. It is
// single-threaded, and a LOCK-less XADD returns exactly the same answers as a
// locked one when nothing races it. A dropped LOCK prefix would leave every
// execution test green and corrupt memory the first time two threads met.
//
// So the encoding layer asserts the prefix directly, by disassembling the compiled
// function and requiring the word "lock" on the instruction that must carry it.
// Between the two, the values are checked by running them and the atomicity is
// checked by reading the bytes.

// --- encoding layer --------------------------------------------------------

// disasmFunc compiles m and disassembles the named function's bytes with llvm-mc,
// returning one normalized string per instruction.
//
// llvm-mc prints a prefix on a line of its own, so "lock" is folded onto the
// instruction it belongs to: that is the whole point of looking at the
// disassembly, and a helper that dropped it would defeat the test.
func disasmFunc(t *testing.T, m *ir.Module, name string) []string {
	t.Helper()
	llvmmc := testenv.Tool(t, "llvm-mc")

	o, err := amd64.CompileToObject(m)
	require.NoError(t, err)

	var code []byte
	var found bool
	for _, s := range o.Syms {
		if s.Name == name && s.Section == obj.SecText {
			code = o.Text[s.Value : s.Value+s.Size]
			found = true
		}
	}
	require.Truef(t, found, "no text symbol %q in the compiled object", name)

	var hexb []string
	for _, b := range code {
		hexb = append(hexb, fmt.Sprintf("0x%02x", b))
	}
	cmd := exec.Command(llvmmc, "--triple=x86_64", "--disassemble")
	cmd.Stdin = strings.NewReader(strings.Join(hexb, " ") + "\n")
	out, err := cmd.Output()
	require.NoErrorf(t, err, "llvm-mc could not disassemble %s: % x", name, code)

	var instrs []string
	pendingPrefix := ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.Join(strings.Fields(line), " ")
		if line == "" || strings.HasPrefix(line, ".") {
			continue
		}
		if line == "lock" || line == "rep" || line == "repne" {
			pendingPrefix = line + " "
			continue
		}
		instrs = append(instrs, pendingPrefix+line)
		pendingPrefix = ""
	}
	require.NotEmptyf(t, instrs, "empty disassembly for %s:\n%s", name, out)
	return instrs
}

// mnemonicOf returns an instruction's mnemonic, keeping any prefix attached, so
// "lock xaddl %r10d, (%rdi)" reports "lock xaddl".
func mnemonicOf(instr string) string {
	fields := strings.Fields(instr)
	if len(fields) >= 2 && fields[0] == "lock" {
		return fields[0] + " " + fields[1]
	}
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// requireSoleMnemonic asserts that exactly one instruction in the function has the
// given mnemonic -- prefix included, which is what makes "lock xaddl" and "xaddl"
// different assertions -- and returns it.
//
// Matching on the mnemonic rather than the whole instruction keeps the assertion
// about the encoding chosen and not about which registers the allocator handed
// out, while still failing if the LOCK prefix goes missing.
func requireSoleMnemonic(t *testing.T, instrs []string, mnemonic string) string {
	t.Helper()
	var hits []string
	for _, instr := range instrs {
		if mnemonicOf(instr) == mnemonic {
			hits = append(hits, instr)
		}
	}
	require.Lenf(t, hits, 1, "expected exactly one %q in:\n%s", mnemonic, strings.Join(instrs, "\n"))
	return hits[0]
}

// atomicUnary builds an exported one-atomic function: `cls name(void *p, cls v)`
// returning the intrinsic's result, or void when the intrinsic has none.
func atomicUnary(m *ir.Module, name, intrinsic string, cls ir.Cls, args int) {
	if args == 1 {
		f := m.NewFunc(name, cls).Export()
		p := f.Param("p", ir.ClsL)
		e := f.Entry()
		e.Ret(e.Intrinsic(intrinsic, cls, p))
		return
	}
	f := m.NewFunc(name, cls).Export()
	p := f.Param("p", ir.ClsL)
	v := f.Param("v", cls)
	e := f.Entry()
	e.Ret(e.Intrinsic(intrinsic, cls, p, v))
}

// TestAtomicIntrinsicsAreAllImplemented is the completeness check: it names every
// atomic intrinsic ir/intrinsic.go registers, the same way registerAtomics builds
// them (four widths crossed with the operations, plus the width-less fence), and
// requires each one both to be registered under that name and to compile.
//
// The registration check is what keeps the list honest. If ir renames or drops one,
// this test fails on the missing registration rather than quietly checking 21
// intrinsics and reporting full coverage.
func TestAtomicIntrinsicsAreAllImplemented(t *testing.T) {
	type intrinsic struct {
		name    string
		cls     ir.Cls
		results bool
		args    int
	}
	var all []intrinsic
	for _, w := range atomicWidths {
		all = append(all,
			intrinsic{"atomic.load." + w.suffix, w.cls, true, 1},
			intrinsic{"atomic.store." + w.suffix, w.cls, false, 2},
			intrinsic{"atomic.xchg." + w.suffix, w.cls, true, 2},
			intrinsic{"atomic.cas." + w.suffix, w.cls, true, 3},
		)
		for _, op := range []string{"add", "sub", "and", "or", "xor"} {
			all = append(all, intrinsic{"atomic." + op + "." + w.suffix, w.cls, true, 2})
		}
	}
	all = append(all, intrinsic{"atomic.fence", ir.ClsW, false, 0})
	// 37, not the 22 AMD64_PARITY_PLAN.md quotes: registerAtomics crosses four
	// widths with nine operations (load, store, xchg, cas, add, sub, and, or, xor)
	// and adds the fence. The count is asserted so that an intrinsic added to ir
	// without a case here fails this test instead of quietly going unimplemented.
	require.Len(t, all, 37, "ir/intrinsic.go registers 37 atomic intrinsics")

	for _, in := range all {
		t.Run(in.name, func(t *testing.T) {
			require.NotNilf(t, ir.LookupIntrinsic(in.name), "%q is not a registered intrinsic", in.name)

			m := ir.NewModule()
			var f *ir.Func
			if in.results {
				f = m.NewFunc("f", in.cls).Export()
			} else {
				f = m.NewFuncVoid("f").Export()
			}
			var args []ir.Ref
			if in.args > 0 {
				args = append(args, f.Param("p", ir.ClsL))
			}
			for i := 1; i < in.args; i++ {
				args = append(args, f.Param(fmt.Sprintf("v%d", i), in.cls))
			}
			e := f.Entry()
			if in.results {
				e.Ret(e.Intrinsic(in.name, in.cls, args...))
			} else {
				e.IntrinsicVoid(in.name, args...)
				e.RetVoid()
			}
			_, err := amd64.CompileObject(m)
			require.NoErrorf(t, err, "%q must compile on amd64", in.name)
		})
	}
}

// TestAtomicEncodingCarriesLockPrefix is the atomicity check. Every intrinsic that
// must be a locked read-modify-write is compiled on its own and its disassembly is
// required to carry the prefix -- because a missing LOCK is invisible to every
// other test in this file.
func TestAtomicEncodingCarriesLockPrefix(t *testing.T) {
	cases := []struct {
		intrinsic string
		cls       ir.Cls
		mnemonic  string
	}{
		// add and sub both go through XADD; sub negates its operand first.
		{"atomic.add.b", ir.ClsW, "lock xaddb"},
		{"atomic.add.h", ir.ClsW, "lock xaddw"},
		{"atomic.add.w", ir.ClsW, "lock xaddl"},
		{"atomic.add.l", ir.ClsL, "lock xaddq"},
		{"atomic.sub.b", ir.ClsW, "lock xaddb"},
		{"atomic.sub.h", ir.ClsW, "lock xaddw"},
		{"atomic.sub.w", ir.ClsW, "lock xaddl"},
		{"atomic.sub.l", ir.ClsL, "lock xaddq"},
		// and/or/xor need the previous value, so they get a locked CMPXCHG loop.
		{"atomic.and.b", ir.ClsW, "lock cmpxchgb"},
		{"atomic.and.h", ir.ClsW, "lock cmpxchgw"},
		{"atomic.and.w", ir.ClsW, "lock cmpxchgl"},
		{"atomic.and.l", ir.ClsL, "lock cmpxchgq"},
		{"atomic.or.b", ir.ClsW, "lock cmpxchgb"},
		{"atomic.or.w", ir.ClsW, "lock cmpxchgl"},
		{"atomic.or.l", ir.ClsL, "lock cmpxchgq"},
		{"atomic.xor.h", ir.ClsW, "lock cmpxchgw"},
		{"atomic.xor.w", ir.ClsW, "lock cmpxchgl"},
		{"atomic.xor.l", ir.ClsL, "lock cmpxchgq"},
	}
	for _, c := range cases {
		t.Run(c.intrinsic, func(t *testing.T) {
			m := ir.NewModule()
			atomicUnary(m, "f", c.intrinsic, c.cls, 2)
			requireSoleMnemonic(t, disasmFunc(t, m, "f"), c.mnemonic)
		})
	}
}

// A compare-and-swap is one locked CMPXCHG, with the comparand in RAX and the
// previous value read back out of it -- no loop and no branch, because CMPXCHG
// reports the previous value on both the success and the failure path.
func TestAtomicEncodingCAS(t *testing.T) {
	widths := []struct {
		intrinsic string
		cls       ir.Cls
		mnemonic  string
	}{
		{"atomic.cas.b", ir.ClsW, "lock cmpxchgb"},
		{"atomic.cas.h", ir.ClsW, "lock cmpxchgw"},
		{"atomic.cas.w", ir.ClsW, "lock cmpxchgl"},
		{"atomic.cas.l", ir.ClsL, "lock cmpxchgq"},
	}
	for _, w := range widths {
		t.Run(w.intrinsic, func(t *testing.T) {
			m := ir.NewModule()
			f := m.NewFunc("f", w.cls).Export()
			p := f.Param("p", ir.ClsL)
			exp := f.Param("e", w.cls)
			nw := f.Param("n", w.cls)
			e := f.Entry()
			e.Ret(e.Intrinsic(w.intrinsic, w.cls, p, exp, nw))

			instrs := disasmFunc(t, m, "f")
			requireSoleMnemonic(t, instrs, w.mnemonic)
			// No retry loop: a CAS reports failure to its caller rather than retrying.
			for _, instr := range instrs {
				require.NotEqual(t, "jne", mnemonicOf(instr),
					"a compare-and-swap must not loop:\n%s", strings.Join(instrs, "\n"))
			}
		})
	}
}

// An exchange is XCHG with no LOCK prefix, and that absence is deliberate: a
// memory-operand XCHG is atomic and a full barrier whether or not the prefix is
// there, so emitting it would only cost a byte. This test pins that decision, so a
// later change that adds the prefix has to be a decision too.
func TestAtomicEncodingXchgIsImplicitlyLocked(t *testing.T) {
	m := ir.NewModule()
	atomicUnary(m, "f", "atomic.xchg.l", ir.ClsL, 2)
	instrs := disasmFunc(t, m, "f")
	requireSoleMnemonic(t, instrs, "xchgq")
	for _, instr := range instrs {
		require.NotEqual(t, "lock xchgq", mnemonicOf(instr))
	}
}

// An atomic load is a plain MOV: acquire ordering is free under TSO, so there is
// no fence, no locked instruction, and nothing to distinguish it from an ordinary
// load. A store is the asymmetric case -- sequential consistency needs the
// store-then-load ordering TSO does not give, so it becomes an XCHG.
func TestAtomicEncodingLoadStoreOrdering(t *testing.T) {
	load := ir.NewModule()
	atomicUnary(load, "f", "atomic.load.l", ir.ClsL, 1)
	loadInstrs := disasmFunc(t, load, "f")
	// The prologue and epilogue move registers around too, so the assertion is
	// about the one instruction that dereferences the pointer: it must be a plain
	// MOV with a memory source, and the function must contain nothing locked and no
	// fence at all.
	dereferences := 0
	for _, instr := range loadInstrs {
		if strings.HasPrefix(instr, "movq (") {
			dereferences++
		}
		require.NotContains(t, mnemonicOf(instr), "lock",
			"an atomic load needs no locked instruction on x86-64")
		require.NotContains(t, mnemonicOf(instr), "xchg",
			"an atomic load needs no exchange on x86-64")
		require.NotEqual(t, "mfence", mnemonicOf(instr),
			"an atomic load needs no fence on x86-64")
	}
	require.Equalf(t, 1, dereferences, "expected one plain MOV through the pointer in:\n%s",
		strings.Join(loadInstrs, "\n"))

	store := ir.NewModule()
	f := store.NewFuncVoid("f").Export()
	p := f.Param("p", ir.ClsL)
	v := f.Param("v", ir.ClsL)
	e := f.Entry()
	e.IntrinsicVoid("atomic.store.l", p, v)
	e.RetVoid()
	storeInstrs := disasmFunc(t, store, "f")
	requireSoleMnemonic(t, storeInstrs, "xchgq")
	for _, instr := range storeInstrs {
		require.NotEqual(t, "mfence", mnemonicOf(instr),
			"XCHG already drains the store buffer; a fence as well would be redundant")
	}
}

// A fence is MFENCE and nothing else. SFENCE and LFENCE would both assemble and
// both be wrong: the reordering a sequentially consistent barrier has to forbid on
// x86-64 is store-then-load, and neither of them forbids it.
func TestAtomicEncodingFence(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFuncVoid("f").Export()
	e := f.Entry()
	e.AtomicFence()
	e.RetVoid()
	requireSoleMnemonic(t, disasmFunc(t, m, "f"), "mfence")
}

// The fetch-and-op loop's shape, asserted once in full: read, then a body that
// recomputes the replacement from what the accumulator now holds, a locked
// CMPXCHG, and a backward branch to the top of that body -- not to the initial
// read, since a failed CMPXCHG has already reloaded the accumulator.
func TestAtomicEncodingFetchOpLoopShape(t *testing.T) {
	m := ir.NewModule()
	atomicUnary(m, "f", "atomic.or.l", ir.ClsL, 2)
	instrs := disasmFunc(t, m, "f")

	var mnemonics []string
	for _, instr := range instrs {
		mnemonics = append(mnemonics, mnemonicOf(instr))
	}
	joined := strings.Join(mnemonics, " ")
	require.Containsf(t, joined, "movq movq orq lock cmpxchgq jne",
		"unexpected fetch-and-or sequence:\n%s", strings.Join(instrs, "\n"))

	// The branch target is the second movq of that run (the start of the loop body),
	// so the displacement spans exactly the three instructions before the jne.
	loopBody := 0
	for i, instr := range instrs {
		if mnemonicOf(instr) == "jne" {
			require.GreaterOrEqual(t, i, 3)
			loopBody = i - 3
		}
	}
	require.Equal(t, "movq", mnemonicOf(instrs[loopBody]),
		"the loop must re-enter at the replacement recomputation")
}

// An atomic on a global goes through a RIP-relative memory operand, and the retry
// loop names that operand twice -- once in the initial read, once in the CMPXCHG.
// Each use needs its own PC-relative relocation; one relocation for two references
// would leave the second instruction addressing whatever the displacement happened
// to be.
func TestAtomicGlobalAddressRelocations(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{Name: "counter", Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{0}}}})
	f := m.NewFunc("f", ir.ClsL).Export()
	e := f.Entry()
	v := f.Param("v", ir.ClsL)
	e.Ret(e.Intrinsic("atomic.or.l", ir.ClsL, f.Sym("counter", 0), v))

	o, err := amd64.CompileToObject(m)
	require.NoError(t, err)

	var count int
	for _, r := range o.Relocs {
		if r.Sym == "counter" {
			require.Equal(t, uint32(obj.R_X86_64_PC32), r.Type)
			count++
		}
	}
	require.Equal(t, 2, count, "each use of the address needs its own relocation")
}

// --- execution layer -------------------------------------------------------

// checker builds a `runtest` body as a chain of assertions: each expect() compares
// a value against what the intrinsic should have produced and returns a distinct
// nonzero code when it does not, so a failing exit status names the assertion that
// failed. The final block returns 0.
type checker struct {
	f    *ir.Func
	cur  *ir.Block
	code int
}

func newChecker(m *ir.Module) *checker {
	f := m.NewFunc("runtest", ir.ClsW).Export()
	return &checker{f: f, cur: f.Entry()}
}

// expect asserts that got equals want, comparing at got's own class.
func (c *checker) expect(got ir.Ref, want ir.Ref) {
	c.code++
	failing := c.code
	bad := c.f.NewBlock(fmt.Sprintf("bad%d", failing))
	next := c.f.NewBlock(fmt.Sprintf("ok%d", failing))
	equal := c.cur.Cmp(ir.CmpEq, ir.ClsW, got, want)
	c.cur.Jnz(equal, next, bad)
	bad.Ret(c.f.Word(int64(failing)))
	c.cur = next
}

// done closes the chain with a successful return and reports how many assertions
// were built, so a test can fail loudly if it accidentally built none.
func (c *checker) done() int {
	c.cur.Ret(c.f.Word(0))
	return c.code
}

// atomicOn applies an intrinsic to an address, at the width the suffix names.
func atomicOn(b *ir.Block, name string, cls ir.Cls, args ...ir.Ref) ir.Ref {
	return b.Intrinsic(name, cls, args...)
}

// storeAt writes an initial value through a pointer at the given access width,
// so each width's atomics start from a known cell.
func storeAt(b *ir.Block, bytes int, addr, val ir.Ref) {
	switch bytes {
	case 1:
		b.StoreSub(ir.SubUB, val, addr)
	case 2:
		b.StoreSub(ir.SubUH, val, addr)
	case 4:
		b.StoreSub(ir.SubW, val, addr)
	default:
		b.StoreSub(ir.SubL, val, addr)
	}
}

// loadAt reads a cell back, zero-extended, to check what an atomic left in memory.
func loadAt(b *ir.Block, bytes int, cls ir.Cls, addr ir.Ref) ir.Ref {
	switch bytes {
	case 1:
		return b.LoadSub(cls, ir.SubUB, addr)
	case 2:
		return b.LoadSub(cls, ir.SubUH, addr)
	case 4:
		return b.LoadSub(cls, ir.SubW, addr)
	default:
		return b.LoadSub(cls, ir.SubL, addr)
	}
}

// atomicWidth pairs an access width with the class its result carries, mirroring
// cc/atomic.go's mapping: the narrow widths compute in a word, only ".l" is long.
type atomicWidth struct {
	suffix string
	bytes  int
	cls    ir.Cls
}

var atomicWidths = []atomicWidth{
	{"b", 1, ir.ClsW},
	{"h", 2, ir.ClsW},
	{"w", 4, ir.ClsW},
	{"l", 8, ir.ClsL},
}

func (w atomicWidth) konst(f *ir.Func, v int64) ir.Ref {
	return f.ConstInt(w.cls, v)
}

// TestAtomicExecLoadStore runs the load/store pair at every width: a store must be
// visible to an ordinary load, and an atomic load must see what an ordinary store
// wrote.
func TestAtomicExecLoadStore(t *testing.T) {
	m := ir.NewModule()
	c := newChecker(m)
	e := c.cur

	for _, w := range atomicWidths {
		p := e.Alloc(8, 8)
		e.Store(c.f.Long(0), p) // clear the whole cell, so a narrow op's neighbours are known
		storeAt(e, w.bytes, p, w.konst(c.f, 0x12))
		c.expect(atomicOn(c.cur, "atomic.load."+w.suffix, w.cls, p), w.konst(c.f, 0x12))

		c.cur.IntrinsicVoid("atomic.store."+w.suffix, p, w.konst(c.f, 0x34))
		c.expect(loadAt(c.cur, w.bytes, w.cls, p), w.konst(c.f, 0x34))
		// A narrow atomic store must not spill into the neighbouring bytes.
		if w.bytes < 8 {
			c.expect(c.cur.LoadSub(ir.ClsL, ir.SubL, p), c.f.Long(0x34))
		}
		e = c.cur
	}
	require.NotZero(t, c.done())
	require.Equal(t, 0, runObj(t, m))
}

// TestAtomicExecXchg checks that an exchange returns the previous value and leaves
// the new one behind, at every width.
func TestAtomicExecXchg(t *testing.T) {
	m := ir.NewModule()
	c := newChecker(m)

	for _, w := range atomicWidths {
		p := c.cur.Alloc(8, 8)
		c.cur.Store(c.f.Long(0), p)
		storeAt(c.cur, w.bytes, p, w.konst(c.f, 0x41))
		c.expect(atomicOn(c.cur, "atomic.xchg."+w.suffix, w.cls, p, w.konst(c.f, 0x59)),
			w.konst(c.f, 0x41))
		c.expect(loadAt(c.cur, w.bytes, w.cls, p), w.konst(c.f, 0x59))
	}
	require.NotZero(t, c.done())
	require.Equal(t, 0, runObj(t, m))
}

// TestAtomicExecFetchOps checks the five fetch-and-op forms at every width: each
// returns the previous value (not the new one -- the distinction the XADD-versus-
// locked-ALU choice turns on) and leaves the computed value in memory.
func TestAtomicExecFetchOps(t *testing.T) {
	ops := []struct {
		name    string
		initial int64
		operand int64
		result  int64
	}{
		{"add", 20, 5, 25},
		{"sub", 20, 5, 15},
		{"and", 0x3c, 0x0f, 0x0c},
		{"or", 0x30, 0x0f, 0x3f},
		{"xor", 0x3c, 0x0f, 0x33},
	}
	m := ir.NewModule()
	c := newChecker(m)

	for _, op := range ops {
		for _, w := range atomicWidths {
			p := c.cur.Alloc(8, 8)
			c.cur.Store(c.f.Long(0), p)
			storeAt(c.cur, w.bytes, p, w.konst(c.f, op.initial))
			previous := atomicOn(c.cur, "atomic."+op.name+"."+w.suffix, w.cls,
				p, w.konst(c.f, op.operand))
			c.expect(previous, w.konst(c.f, op.initial))
			c.expect(loadAt(c.cur, w.bytes, w.cls, p), w.konst(c.f, op.result))
		}
	}
	require.NotZero(t, c.done())
	require.Equal(t, 0, runObj(t, m))
}

// TestAtomicExecCAS covers both CAS paths at every width. The failure path is the
// one worth being explicit about: it must return the value it found and leave
// memory untouched, which on x86-64 falls out of CMPXCHG rather than needing a
// branch -- so a mistake there would show up only here.
func TestAtomicExecCAS(t *testing.T) {
	m := ir.NewModule()
	c := newChecker(m)

	for _, w := range atomicWidths {
		p := c.cur.Alloc(8, 8)
		c.cur.Store(c.f.Long(0), p)
		storeAt(c.cur, w.bytes, p, w.konst(c.f, 0x11))

		// Success: comparand matches, so the swap happens and the previous value comes
		// back (which is also how a caller detects success -- previous == expected).
		swapped := atomicOn(c.cur, "atomic.cas."+w.suffix, w.cls,
			p, w.konst(c.f, 0x11), w.konst(c.f, 0x63))
		c.expect(swapped, w.konst(c.f, 0x11))
		c.expect(loadAt(c.cur, w.bytes, w.cls, p), w.konst(c.f, 0x63))

		// Failure: comparand does not match, so nothing is stored and the value found
		// is returned.
		kept := atomicOn(c.cur, "atomic.cas."+w.suffix, w.cls,
			p, w.konst(c.f, 0x11), w.konst(c.f, 0x7f))
		c.expect(kept, w.konst(c.f, 0x63))
		c.expect(loadAt(c.cur, w.bytes, w.cls, p), w.konst(c.f, 0x63))
	}
	require.NotZero(t, c.done())
	require.Equal(t, 0, runObj(t, m))
}

// TestAtomicExecNarrowEdges pushes the byte and halfword widths to their edges. The
// previous value an atomic yields is zero-extended (matching arm64's LDAXRB/LDAXRH),
// and a narrow read-modify-write wraps within its own width without touching the
// bytes beside it -- which is exactly what an 8- or 16-bit XADD does and what a
// 32-bit one used by mistake would not.
func TestAtomicExecNarrowEdges(t *testing.T) {
	m := ir.NewModule()
	c := newChecker(m)

	// A byte at 0xff yields 0xff, not -1, and adding 1 wraps to 0.
	{
		p := c.cur.Alloc(8, 8)
		c.cur.Store(c.f.Long(-1), p) // every byte 0xff
		c.expect(atomicOn(c.cur, "atomic.load.b", ir.ClsW, p), c.f.Word(0xff))
		c.expect(atomicOn(c.cur, "atomic.add.b", ir.ClsW, p, c.f.Word(1)), c.f.Word(0xff))
		c.expect(c.cur.LoadSub(ir.ClsW, ir.SubUB, p), c.f.Word(0))
		// The wrap must not have carried into byte 1, which is still 0xff.
		c.expect(c.cur.LoadSub(ir.ClsW, ir.SubUH, p), c.f.Word(0xff00))
	}
	// A halfword at 0xffff behaves the same way, and 0 - 1 wraps back to 0xffff.
	{
		p := c.cur.Alloc(8, 8)
		c.cur.Store(c.f.Long(0), p)
		c.cur.StoreSub(ir.SubUH, c.f.Word(0xffff), p)
		c.expect(atomicOn(c.cur, "atomic.load.h", ir.ClsW, p), c.f.Word(0xffff))
		c.expect(atomicOn(c.cur, "atomic.add.h", ir.ClsW, p, c.f.Word(1)), c.f.Word(0xffff))
		c.expect(c.cur.LoadSub(ir.ClsW, ir.SubUH, p), c.f.Word(0))
		c.expect(atomicOn(c.cur, "atomic.sub.h", ir.ClsW, p, c.f.Word(1)), c.f.Word(0))
		c.expect(c.cur.LoadSub(ir.ClsW, ir.SubUH, p), c.f.Word(0xffff))
		// Bytes 2..7 were zero and stay zero.
		c.expect(c.cur.LoadSub(ir.ClsL, ir.SubL, p), c.f.Long(0xffff))
	}
	// A byte-width CAS compares only the byte: a cell whose other bytes differ from
	// the comparand must still swap.
	{
		p := c.cur.Alloc(8, 8)
		c.cur.Store(c.f.Long(-256), p) // low byte 0x00, everything above it 0xff
		c.expect(atomicOn(c.cur, "atomic.cas.b", ir.ClsW, p, c.f.Word(0), c.f.Word(0x5a)),
			c.f.Word(0))
		c.expect(c.cur.LoadSub(ir.ClsW, ir.SubUB, p), c.f.Word(0x5a))
		c.expect(c.cur.LoadSub(ir.ClsL, ir.SubL, p), c.f.Long(-256+0x5a))
	}
	// And a byte-width fetch-and-and, whose CMPXCHG loop must keep its accumulator
	// zero-extended across an iteration.
	{
		p := c.cur.Alloc(8, 8)
		c.cur.Store(c.f.Long(-1), p)
		c.expect(atomicOn(c.cur, "atomic.and.b", ir.ClsW, p, c.f.Word(0x0f)), c.f.Word(0xff))
		c.expect(c.cur.LoadSub(ir.ClsW, ir.SubUB, p), c.f.Word(0x0f))
		c.expect(c.cur.LoadSub(ir.ClsL, ir.SubL, p), c.f.Long(-256+0x0f))
	}
	require.NotZero(t, c.done())
	require.Equal(t, 0, runObj(t, m))
}

// TestAtomicExecWideEdges checks the 64-bit width at the boundaries a 32-bit
// operation would get wrong: a value above 2^32, the sign bit, and wraparound past
// the top of the range.
func TestAtomicExecWideEdges(t *testing.T) {
	m := ir.NewModule()
	c := newChecker(m)

	// A value that does not fit in 32 bits survives a load, an exchange and a CAS.
	{
		const high = int64(0x1122334455667788)
		p := c.cur.Alloc(8, 8)
		c.cur.Store(c.f.Long(high), p)
		c.expect(atomicOn(c.cur, "atomic.load.l", ir.ClsL, p), c.f.Long(high))
		c.expect(atomicOn(c.cur, "atomic.xchg.l", ir.ClsL, p, c.f.Long(-1)), c.f.Long(high))
		c.expect(atomicOn(c.cur, "atomic.cas.l", ir.ClsL, p, c.f.Long(-1), c.f.Long(high)),
			c.f.Long(-1))
		c.expect(c.cur.LoadSub(ir.ClsL, ir.SubL, p), c.f.Long(high))
	}
	// -1 + 1 wraps to 0, and 0 - 1 back to -1: the XADD path at full width.
	{
		p := c.cur.Alloc(8, 8)
		c.cur.Store(c.f.Long(-1), p)
		c.expect(atomicOn(c.cur, "atomic.add.l", ir.ClsL, p, c.f.Long(1)), c.f.Long(-1))
		c.expect(c.cur.LoadSub(ir.ClsL, ir.SubL, p), c.f.Long(0))
		c.expect(atomicOn(c.cur, "atomic.sub.l", ir.ClsL, p, c.f.Long(1)), c.f.Long(0))
		c.expect(c.cur.LoadSub(ir.ClsL, ir.SubL, p), c.f.Long(-1))
	}
	// The sign bit through the bitwise forms, whose loop computes at 64 bits.
	{
		const signBit = int64(-1) << 63
		p := c.cur.Alloc(8, 8)
		c.cur.Store(c.f.Long(0), p)
		c.expect(atomicOn(c.cur, "atomic.or.l", ir.ClsL, p, c.f.Long(signBit)), c.f.Long(0))
		c.expect(c.cur.LoadSub(ir.ClsL, ir.SubL, p), c.f.Long(signBit))
		c.expect(atomicOn(c.cur, "atomic.xor.l", ir.ClsL, p, c.f.Long(-1)),
			c.f.Long(signBit))
		c.expect(c.cur.LoadSub(ir.ClsL, ir.SubL, p), c.f.Long(^signBit))
		c.expect(atomicOn(c.cur, "atomic.and.l", ir.ClsL, p, c.f.Long(signBit)),
			c.f.Long(^signBit))
		c.expect(c.cur.LoadSub(ir.ClsL, ir.SubL, p), c.f.Long(0))
	}
	require.NotZero(t, c.done())
	require.Equal(t, 0, runObj(t, m))
}

// TestAtomicExecFence runs a fence, which has no observable result in a
// single-threaded program. What this establishes is only that MFENCE is encoded
// correctly enough to execute -- the ordering it provides is not testable here.
func TestAtomicExecFence(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("runtest", ir.ClsW).Export()
	e := f.Entry()
	p := e.Alloc(8, 8)
	e.AtomicStore(ir.ClsL, p, f.Long(7))
	e.AtomicFence()
	value := e.AtomicLoad(ir.ClsL, p)
	e.AtomicFence()
	e.Ret(e.Extuw(ir.ClsW, e.Sub(ir.ClsL, value, f.Long(7))))
	require.Equal(t, 0, runObj(t, m))
}

// TestAtomicExecOnGlobal exercises the RIP-relative address path end to end,
// including the CMPXCHG loop's two relocations against the same symbol.
func TestAtomicExecOnGlobal(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data, &ir.Data{Name: "gcounter", Align: 8, Items: []ir.DataItem{{Sub: ir.SubL, Ints: []int64{0}}}})
	c := newChecker(m)
	g := c.f.Sym("gcounter", 0)

	c.cur.IntrinsicVoid("atomic.store.l", g, c.f.Long(0x30))
	c.expect(atomicOn(c.cur, "atomic.load.l", ir.ClsL, g), c.f.Long(0x30))
	c.expect(atomicOn(c.cur, "atomic.add.l", ir.ClsL, g, c.f.Long(2)), c.f.Long(0x30))
	c.expect(atomicOn(c.cur, "atomic.or.l", ir.ClsL, g, c.f.Long(0x0f)), c.f.Long(0x32))
	c.expect(atomicOn(c.cur, "atomic.load.l", ir.ClsL, g), c.f.Long(0x3f))
	c.expect(atomicOn(c.cur, "atomic.cas.l", ir.ClsL, g, c.f.Long(0x3f), c.f.Long(1)),
		c.f.Long(0x3f))
	c.expect(atomicOn(c.cur, "atomic.load.l", ir.ClsL, g), c.f.Long(1))
	require.NotZero(t, c.done())
	require.Equal(t, 0, runObj(t, m))
}

// TestAtomicExecUnderRegisterPressure keeps more values live than the allocator has
// registers across each atomic, and reaches memory through a *computed* pointer
// rather than an alloca, so the address itself is an ordinary temp the allocator may
// spill.
//
// That combination is the interesting one. An alloca address is rematerialized as
// rbp+offset and folds straight into the memory operand, so it never occupies a
// register; a computed pointer under pressure lands in a spill slot and has to be
// reloaded into the one scratch register the sequence has left, then stay there
// across the whole CMPXCHG loop while the loop uses RAX, RCX and the operand's own
// register. Verified by inspection that the compiled loop does reload the address
// once, before the loop, and does not clobber it inside.
func TestAtomicExecUnderRegisterPressure(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("runtest", ir.ClsW).Export()
	e := f.Entry()

	base := e.Alloc(16, 16)
	e.Store(f.Long(0), base)
	// A computed pointer to the second word, so the atomics run on an address the
	// allocator has to keep somewhere.
	p := e.Add(ir.ClsL, base, f.Long(8))
	e.Store(f.Long(100), p)

	// Fourteen values live across every atomic below, against nine allocatable GPRs.
	var live []ir.Ref
	for i := 0; i < 14; i++ {
		live = append(live, e.Add(ir.ClsL, f.Long(int64(i)), f.Long(1)))
	}

	previousAdd := e.Intrinsic("atomic.add.l", ir.ClsL, p, f.Long(5)) // 100 -> 105
	previousOr := e.Intrinsic("atomic.or.l", ir.ClsL, p, f.Long(2))   // 105 -> 107
	previousCAS := e.Intrinsic("atomic.cas.l", ir.ClsL, p, f.Long(107), f.Long(9))
	previousXchg := e.Intrinsic("atomic.xchg.l", ir.ClsL, p, f.Long(4)) // 9 -> 4

	// 100 + 105 + 107 + 9 == 321, plus 1+2+...+14 == 105, plus the final cell 4.
	sum := e.Add(ir.ClsL, e.Add(ir.ClsL, previousAdd, previousOr),
		e.Add(ir.ClsL, previousCAS, previousXchg))
	for _, value := range live {
		sum = e.Add(ir.ClsL, sum, value)
	}
	sum = e.Add(ir.ClsL, sum, e.LoadSub(ir.ClsL, ir.SubL, p))
	// The first word must be untouched: every atomic addressed the second one.
	sum = e.Add(ir.ClsL, sum, e.LoadSub(ir.ClsL, ir.SubL, base))
	e.Ret(e.Extuw(ir.ClsW, e.Sub(ir.ClsL, sum, f.Long(321+105+4))))

	require.Equal(t, 0, runObj(t, m))
}

// TestAtomicVoidFormUsesLockedALU covers the void read-modify-write: an intrinsic
// written with no result temp, which the textual IL can express and which collapses
// to a single locked memory-destination instruction instead of a CMPXCHG loop.
func TestAtomicVoidFormUsesLockedALU(t *testing.T) {
	ops := []struct {
		name     string
		mnemonic string
	}{
		{"add", "lock addq"},
		{"sub", "lock subq"},
		{"and", "lock andq"},
		{"or", "lock orq"},
		{"xor", "lock xorq"},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			m := ir.NewModule()
			f := m.NewFuncVoid("f").Export()
			p := f.Param("p", ir.ClsL)
			v := f.Param("v", ir.ClsL)
			e := f.Entry()
			e.IntrinsicVoid("atomic."+op.name+".l", p, v)
			e.RetVoid()

			instrs := disasmFunc(t, m, "f")
			requireSoleMnemonic(t, instrs, op.mnemonic)
			for _, instr := range instrs {
				require.NotContains(t, mnemonicOf(instr), "cmpxchg",
					"a void fetch-and-op needs no loop:\n%s", strings.Join(instrs, "\n"))
			}
		})
	}
}

// And it must still perform the operation, not merely encode one.
func TestAtomicExecVoidForm(t *testing.T) {
	m := ir.NewModule()
	c := newChecker(m)

	for _, w := range atomicWidths {
		p := c.cur.Alloc(8, 8)
		c.cur.Store(c.f.Long(0), p)
		storeAt(c.cur, w.bytes, p, w.konst(c.f, 0x3c))
		c.cur.IntrinsicVoid("atomic.and."+w.suffix, p, w.konst(c.f, 0x0f))
		c.expect(loadAt(c.cur, w.bytes, w.cls, p), w.konst(c.f, 0x0c))
		c.cur.IntrinsicVoid("atomic.add."+w.suffix, p, w.konst(c.f, 3))
		c.expect(loadAt(c.cur, w.bytes, w.cls, p), w.konst(c.f, 0x0f))
		c.cur.IntrinsicVoid("atomic.sub."+w.suffix, p, w.konst(c.f, 5))
		c.expect(loadAt(c.cur, w.bytes, w.cls, p), w.konst(c.f, 0x0a))
		c.cur.IntrinsicVoid("atomic.xchg."+w.suffix, p, w.konst(c.f, 0x77))
		c.expect(loadAt(c.cur, w.bytes, w.cls, p), w.konst(c.f, 0x77))
	}
	require.NotZero(t, c.done())
	require.Equal(t, 0, runObj(t, m))
}

// TestAtomicExecFromC runs the C atomic builtins natively, which is the path a real
// program takes: cc/atomic.go picks the intrinsic name and width from the pointee
// type, so this checks that the frontend's choices and this backend's lowering agree
// on all four widths at once -- including the two forms that are not a bare
// intrinsic (add_fetch, which adds the operand back to the previous value, and
// compare_exchange_n, which stores the value seen back through the expected
// pointer).
func TestAtomicExecFromC(t *testing.T) {
	m, err := cc.CompileFor(cc.TargetAMD64, "atomics.c", `
int runtest(void) {
	int i = 10;
	if (__atomic_fetch_add(&i, 5, __ATOMIC_SEQ_CST) != 10) return 1;   /* old */
	if (i != 15) return 2;
	if (__atomic_add_fetch(&i, 5, __ATOMIC_SEQ_CST) != 20) return 3;   /* new */
	if (__atomic_fetch_sub(&i, 3, __ATOMIC_SEQ_CST) != 20) return 4;   /* i=17 */
	if (__atomic_fetch_and(&i, 0xF, __ATOMIC_SEQ_CST) != 17) return 5; /* i=1 */
	if (__atomic_fetch_or(&i, 4, __ATOMIC_SEQ_CST) != 1) return 6;     /* i=5 */
	if (__atomic_fetch_xor(&i, 1, __ATOMIC_SEQ_CST) != 5) return 7;    /* i=4 */
	if (i != 4) return 8;

	__atomic_store_n(&i, 42, __ATOMIC_SEQ_CST);
	if (__atomic_load_n(&i, __ATOMIC_SEQ_CST) != 42) return 9;
	if (__atomic_exchange_n(&i, 7, __ATOMIC_SEQ_CST) != 42) return 10;
	if (i != 7) return 11;

	int exp = 7;
	if (!__atomic_compare_exchange_n(&i, &exp, 99, 0, __ATOMIC_SEQ_CST, __ATOMIC_SEQ_CST)) return 12;
	if (i != 99) return 13;
	exp = 7; /* mismatch: no swap, and exp is updated to the value seen */
	if (__atomic_compare_exchange_n(&i, &exp, 1, 0, __ATOMIC_SEQ_CST, __ATOMIC_SEQ_CST)) return 14;
	if (exp != 99 || i != 99) return 15;

	long L = 1000;
	if (__atomic_fetch_add(&L, 500, __ATOMIC_SEQ_CST) != 1000 || L != 1500) return 16;
	unsigned char b = 200;
	if (__atomic_fetch_add(&b, 100, __ATOMIC_SEQ_CST) != 200 || b != (unsigned char)44) return 17;
	unsigned short h = 0xFF00;
	if (__atomic_fetch_or(&h, 0xFF, __ATOMIC_SEQ_CST) != 0xFF00 || h != 0xFFFF) return 18;

	__atomic_thread_fence(__ATOMIC_SEQ_CST);
	return 0;
}`)
	require.NoError(t, err)
	require.Equal(t, 0, runObj(t, m))
}

// An atomic name the backend does not implement must say so rather than compile to
// something plausible. The registered set is closed, so this can only come from
// hand-written IL -- which is exactly when a clear error matters.
func TestAtomicUnknownWidthIsRefused(t *testing.T) {
	for _, name := range []string{"atomic.add.q", "atomic.nand.w", "atomic.load"} {
		m := ir.NewModule()
		f := m.NewFunc("f", ir.ClsL).Export()
		p := f.Param("p", ir.ClsL)
		e := f.Entry()
		e.Ret(e.Intrinsic(name, ir.ClsL, p, f.Long(1)))

		_, err := amd64.CompileObject(m)
		require.Errorf(t, err, "%q must not compile", name)
		require.Contains(t, err.Error(), name)
	}
}
