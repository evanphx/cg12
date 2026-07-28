package amd64_test

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/internal/testenv"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/require"
)

// This file covers the 128-bit memory ops, ir.OLoadq and ir.OStoreq, in two
// layers: what bytes come out (below, through llvm-mc) and what the bytes do when
// executed (further down, through runObj on this x86-64 host).
//
// Both layers are needed and neither substitutes for the other. A 16-byte access
// that moves only its low eight bytes still passes any test that inspects the low
// half, and an execution test cannot tell a MOVDQU from a MOVDQA that happened to
// be handed an aligned address -- while an encoding test cannot tell whether the
// address the instruction names is the right one. So the execution tests below
// always check both halves, deliberately use misaligned addresses, and put a value
// with its top bit set in the high half.

// --- encoding layer --------------------------------------------------------

// wideAsm compiles m and returns the disassembly of function `name`, one
// normalized instruction per line.
//
// amd64 carries no in-package disassembler (there is no x64.Decode to mirror
// arm64's a64.Decode), so llvm-mc is the oracle, as it already is for the encoders
// one level down in amd64/x64. Going through the whole backend rather than calling
// the encoder directly is the point: it is what checks that the address the fold
// put on the instruction is the address the emitter encoded.
func wideAsm(t *testing.T, m *ir.Module, name string) []string {
	t.Helper()
	llvmMC := testenv.Tool(t, "llvm-mc")

	o, err := amd64.CompileToObject(m)
	require.NoError(t, err)

	var code []byte
	found := false
	for _, s := range o.Syms {
		if s.Name == name && s.Section == obj.SecText {
			code = o.Text[s.Value : s.Value+s.Size]
			found = true
			break
		}
	}
	require.Truef(t, found, "no .text symbol %q in the compiled object", name)

	var hexb []string
	for _, b := range code {
		hexb = append(hexb, fmt.Sprintf("0x%02x", b))
	}
	cmd := exec.Command(llvmMC, "--triple=x86_64", "--disassemble")
	cmd.Stdin = strings.NewReader(strings.Join(hexb, " ") + "\n")
	out, err := cmd.Output()
	require.NoErrorf(t, err, "llvm-mc could not disassemble %s:\n% x", name, code)

	var instrs []string
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.Join(strings.Fields(line), " ")
		if line == "" || strings.HasPrefix(line, ".") {
			continue
		}
		instrs = append(instrs, line)
	}
	return instrs
}

// requireInstrLike asserts that some disassembled instruction matches pattern,
// printing the whole function when none does -- the useful thing to see on a
// failure.
//
// The pattern leaves the registers open, because which ones an access uses is the
// allocator's choice and not what any test here is about: `%g` stands for any
// general-purpose register and `%v` for any XMM register. Everything else -- the
// mnemonic, the displacement, the index scale -- is matched literally, and that is
// the part being checked.
func requireInstrLike(t *testing.T, instrs []string, pattern string) {
	t.Helper()
	rx := regexp.QuoteMeta(pattern)
	rx = strings.ReplaceAll(rx, "%g", "%[a-z][a-z0-9]*")
	rx = strings.ReplaceAll(rx, "%v", "%xmm[0-9]+")
	re := regexp.MustCompile("^" + rx + "$")
	for _, got := range instrs {
		if re.MatchString(got) {
			return
		}
	}
	t.Fatalf("expected an instruction like %q among:\n\t%s", pattern, strings.Join(instrs, "\n\t"))
}

// requireNoInstrPrefix asserts that no disassembled instruction starts with the
// given mnemonic. It is how "the value did not travel through a scalar move" is
// stated: a movss or movsd anywhere near a 128-bit value is a four- or eight-byte
// move of a sixteen-byte thing.
func requireNoInstrPrefix(t *testing.T, instrs []string, mnemonic string) {
	t.Helper()
	for _, got := range instrs {
		if strings.HasPrefix(got, mnemonic+" ") {
			t.Fatalf("unexpected %s in:\n\t%s", mnemonic, strings.Join(instrs, "\n\t"))
		}
	}
}

// wideCopyModule builds `runtest` as a 128-bit copy from src to dst, where each
// address is the given alloca offset away from its base. It is the shape every
// encoding test below wants, differing only in the addresses.
func wideCopyModule(loadOff, storeOff int64) *ir.Module {
	m := ir.NewModule()
	f, e := entry(m)
	src := e.Alloc(16, 64)
	dst := e.Alloc(16, 64)
	from, to := src, dst
	if loadOff != 0 {
		from = e.Add(ir.ClsL, src, f.Long(loadOff))
	}
	if storeOff != 0 {
		to = e.Add(ir.ClsL, dst, f.Long(storeOff))
	}
	e.Store(e.Load(ir.ClsQ, from), to)
	e.Ret(f.Word(0))
	return m
}

// TestWideLoadStoreEncoding is the base case: a 128-bit load and store of an
// alloca address encode as MOVDQU (never MOVDQA, which would fault on a
// misaligned address) and never as a scalar move.
func TestWideLoadStoreEncoding(t *testing.T) {
	instrs := wideAsm(t, wideCopyModule(0, 0), "runtest")
	requireInstrLike(t, instrs, "movdqu (%g), %v")
	requireInstrLike(t, instrs, "movdqu %v, (%g)")
	requireNoInstrPrefix(t, instrs, "movdqa")
	requireNoInstrPrefix(t, instrs, "movss")
	requireNoInstrPrefix(t, instrs, "movsd")
}

// TestWideDisplacementFoldsIntoTheAccess is the first half of what removing
// foldaddr.go's 128-bit skip buys: `base + constant` becomes the instruction's
// displacement instead of a separate lea, and -- the part that would have been a
// silent wrong answer before -- the displacement actually reaches the encoding
// rather than being dropped the way the scalar FP path would drop it.
func TestWideDisplacementFoldsIntoTheAccess(t *testing.T) {
	instrs := wideAsm(t, wideCopyModule(16, 32), "runtest")
	requireInstrLike(t, instrs, "movdqu 16(%g), %v")
	requireInstrLike(t, instrs, "movdqu %v, 32(%g)")
	// The `+ constant` was consumed, not merely duplicated: no add survives, and
	// the accesses do not go to the unfolded base. Before the skip was lifted the
	// address arrived in a register from a separate addq.
	requireNoInstrPrefix(t, instrs, "addq")
	requireNoInstrPrefix(t, instrs, "addl")

	// The same shape past the disp8 boundary, so the mod=2 (disp32) encoding is
	// covered as well as mod=1.
	far := wideAsm(t, wideCopyModule(256, 4096), "runtest")
	requireInstrLike(t, far, "movdqu 256(%g), %v")
	requireInstrLike(t, far, "movdqu %v, 4096(%g)")
}

// TestWideIndexedAddressFoldsIntoTheAccess is the second half: an alloca base plus
// a scaled runtime index becomes one SIB operand on the access itself, on the store
// side as well as the load side (the store's index sits one further along its
// operand list, past the value, which is the easy thing to get wrong). The indices
// are parameters, so nothing can fold them to constants.
func TestWideIndexedAddressFoldsIntoTheAccess(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("wideidx", ir.ClsW).Export()
	i := f.Param("i", ir.ClsL)
	j := f.Param("j", ir.ClsL)
	e := f.Entry()
	buf := e.Alloc(16, 256)
	// The two accesses use different scales, so a scale read off the wrong operand
	// cannot pass by coincidence.
	v := e.Load(ir.ClsQ, e.Add(ir.ClsL, buf, e.Shl(ir.ClsL, i, f.Long(3))))
	e.Store(v, e.Add(ir.ClsL, buf, e.Shl(ir.ClsL, j, f.Long(2))))
	e.Ret(f.Word(0))

	instrs := wideAsm(t, m, "wideidx")
	requireInstrLike(t, instrs, "movdqu (%g,%g,8), %v")
	requireInstrLike(t, instrs, "movdqu %v, (%g,%g,4)")
	// Both shifts were absorbed into the scale rather than computed.
	requireNoInstrPrefix(t, instrs, "shlq")
}

// A shift the SIB byte cannot express is left alone: x86 scales by 1, 2, 4 or 8
// only, so an array of 128-bit elements keeps a doubling shift and scales by 8.
func TestWideIndexScaleBeyondEightKeepsTheShift(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("wideidx16", ir.ClsW).Export()
	i := f.Param("i", ir.ClsL)
	e := f.Entry()
	buf := e.Alloc(16, 256)
	off := e.Shl(ir.ClsL, i, f.Long(1)) // i*2, then the fold's scale of 8 -> i*16
	e.Store(e.Load(ir.ClsQ, e.Add(ir.ClsL, buf, e.Shl(ir.ClsL, off, f.Long(3)))), buf)
	e.Ret(f.Word(0))

	instrs := wideAsm(t, m, "wideidx16")
	requireInstrLike(t, instrs, "movdqu (%g,%g,8), %v")
	requireInstrLike(t, instrs, "shlq $1, %g")
	for _, got := range instrs {
		require.False(t, strings.HasPrefix(got, "shlq $3,"), "the scalable shift should have been folded")
		require.False(t, strings.HasPrefix(got, "shlq $4,"), "the shift pair should not have been merged")
	}
}

// wideZeroData is a 16-byte, 16-aligned global for the symbol-addressing tests.
func wideZeroData(name string) *ir.Data {
	return &ir.Data{Name: name, Align: 16, Items: []ir.DataItem{{Zero: 16}}}
}

// TestWideSymbolAddressIsRIPRelative covers the third addressing form memFor can
// produce: a direct global, which becomes [rip + disp32] plus a PC32 relocation.
// The relocation is written at the instruction's last four bytes, which is only
// correct because MOVDQU carries no immediate after its displacement.
func TestWideSymbolAddressIsRIPRelative(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data, wideZeroData("widesrc"), wideZeroData("widedst"))
	f, e := entry(m)
	e.Store(e.Load(ir.ClsQ, f.Sym("widesrc", 0)), f.Sym("widedst", 0))
	e.Ret(f.Word(0))

	instrs := wideAsm(t, m, "runtest")
	requireInstrLike(t, instrs, "movdqu (%rip), %v")
	requireInstrLike(t, instrs, "movdqu %v, (%rip)")

	o, err := amd64.CompileToObject(m)
	require.NoError(t, err)
	var syms []string
	for _, r := range o.Relocs {
		syms = append(syms, r.Sym)
		require.Equal(t, uint32(obj.R_X86_64_PC32), r.Type)
		require.EqualValues(t, -4, r.Addend)
	}
	require.Equal(t, []string{"widesrc", "widedst"}, syms)
}

// TestWidePhiCopyIsAWholeVectorMove pins the copy that landing OLoadq makes
// reachable. SSA destruction turns a 128-bit phi into an ir.OCopy whose class is
// ir.ClsQ; the generic float path would have sized it at four bytes and emitted
// MOVSS, moving a quarter of the value. It must be MOVDQA.
func TestWidePhiCopyIsAWholeVectorMove(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("widephi", ir.ClsW).Export()
	sel := f.Param("sel", ir.ClsW)
	start := f.Entry()
	left := f.NewBlock("left")
	right := f.NewBlock("right")
	join := f.NewBlock("join")

	buf := start.Alloc(16, 64)
	start.Jnz(sel, left, right)
	a := left.Load(ir.ClsQ, buf)
	left.Goto(join)
	b := right.Load(ir.ClsQ, right.Add(ir.ClsL, buf, f.Long(16)))
	right.Goto(join)
	v := join.Phi(ir.ClsQ, ir.PhiEdge{From: left, Val: a}, ir.PhiEdge{From: right, Val: b})
	join.Store(v, join.Add(ir.ClsL, buf, f.Long(32)))
	join.Ret(f.Word(0))

	instrs := wideAsm(t, m, "widephi")
	requireNoInstrPrefix(t, instrs, "movss")
	requireNoInstrPrefix(t, instrs, "movsd")
	// Either the phi's two inputs were coalesced onto one register (no copy at all)
	// or a copy was emitted -- but if one was, it is the whole vector.
	for _, got := range instrs {
		if strings.HasPrefix(got, "mov") && strings.Contains(got, "%xmm") {
			require.Truef(t, strings.HasPrefix(got, "movdqu ") || strings.HasPrefix(got, "movdqa "),
				"128-bit value moved by %q, in:\n\t%s", got, strings.Join(instrs, "\n\t"))
		}
	}
}

// A 128-bit value has nowhere to live but a register: every amd64 spill slot is
// eight bytes, so a sixteen-byte home does not exist. Keeping one across a call is
// therefore refused -- System V has no callee-saved XMM, so a call-crossing 128-bit
// value has no register either, and colouring gives it a slot.
//
// The refusal is what makes the limitation safe rather than silent. The generic
// paths would all have taken a scalar branch (a ClsQ temp reports IsFloat, and its
// size is neither 8 nor 4, so the emitter's spillReg and the loc-based move both
// fall through to MOVSS) and kept four bytes of sixteen.
func TestWideValueAcrossACallIsRefused(t *testing.T) {
	m := ir.NewModule()
	h := m.NewFunc("sink", ir.ClsW)
	h.Entry().Ret(h.Word(0))

	f, e := entry(m)
	buf := e.Alloc(16, 32)
	v := e.Load(ir.ClsQ, buf) // live across the call below
	e.Call(ir.ClsW, f.Sym("sink", 0))
	e.Store(v, e.Add(ir.ClsL, buf, f.Long(16)))
	e.Ret(f.Word(0))

	_, err := amd64.CompileObject(m)
	require.Error(t, err, "a 128-bit value crossing a call has no 16-byte home")
	require.Contains(t, err.Error(), "128-bit")
	require.Contains(t, err.Error(), "8-byte frame slot")
}

// --- execution layer -------------------------------------------------------

// wideConst is a 128-bit test value whose halves are distinguishable and whose
// high half has its top bit set: a load or store that sign-extends, truncates, or
// swaps halves cannot produce it by accident.
const (
	wideLo = int64(0x1122334455667788)
	wideHi = int64(-0x6655443322110100) // 0x99AABBCCDDEEFF00
)

// checkHalves appends the comparison of the two 64-bit halves at addr against
// wideLo/wideHi, returning a word that is 0 exactly when both match.
func checkHalves(f *ir.Func, e *ir.Block, addr ir.Ref) ir.Ref {
	lo := e.Load(ir.ClsL, addr)
	hi := e.Load(ir.ClsL, e.Add(ir.ClsL, addr, f.Long(8)))
	badLo := e.Cmp(ir.CmpNe, ir.ClsW, lo, f.Long(wideLo))
	badHi := e.Cmp(ir.CmpNe, ir.ClsW, hi, f.Long(wideHi))
	// 1 for a wrong low half, 2 for a wrong high half, 3 for both, so a failure
	// says which half went missing rather than only that something did.
	return e.Add(ir.ClsW, badLo, e.Mul(ir.ClsW, badHi, f.Word(2)))
}

// fillWide writes the test value's two halves at addr with ordinary 64-bit stores,
// so the 128-bit op under test is the only 128-bit thing in the program.
func fillWide(f *ir.Func, e *ir.Block, addr ir.Ref) {
	e.Store(f.Long(wideLo), addr)
	e.Store(f.Long(wideHi), e.Add(ir.ClsL, addr, f.Long(8)))
}

// TestObjWideRoundTrip moves a 128-bit value through memory and verifies both
// halves at the destination. The high half is what a broken implementation loses:
// a test reading only the low 64 bits passes with the top half never written at
// all.
func TestObjWideRoundTrip(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	src := e.Alloc(16, 16)
	dst := e.Alloc(16, 16)
	fillWide(f, e, src)
	e.Store(e.Load(ir.ClsQ, src), dst)
	e.Ret(checkHalves(f, e, dst))
	require.Equal(t, 0, runObj(t, m))
}

// TestObjWideUnalignedAccess is why the memory forms are MOVDQU: both the load and
// the store address are deliberately misaligned (odd, and 16k+3), which MOVDQA
// would answer with #GP -- a SIGSEGV, not a wrong value.
func TestObjWideUnalignedAccess(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	buf := e.Alloc(16, 64)
	src := e.Add(ir.ClsL, buf, f.Long(1))
	fillWide(f, e, src)
	dst := e.Add(ir.ClsL, buf, f.Long(35)) // 32 + 3: past src's 17 bytes, and odd
	e.Store(e.Load(ir.ClsQ, e.Add(ir.ClsL, buf, f.Long(1))), dst)
	e.Ret(checkHalves(f, e, e.Add(ir.ClsL, buf, f.Long(35))))
	require.Equal(t, 0, runObj(t, m))
}

// TestObjWideDisplacedAccess exercises the folded [base + disp] form at run time:
// both the load and the store carry a nonzero displacement, and the displacement
// is large enough (past 127) to force a disp32 rather than a disp8.
func TestObjWideDisplacedAccess(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	buf := e.Alloc(16, 512)
	src := e.Add(ir.ClsL, buf, f.Long(256))
	fillWide(f, e, src)
	e.Store(e.Load(ir.ClsQ, e.Add(ir.ClsL, buf, f.Long(256))),
		e.Add(ir.ClsL, buf, f.Long(384)))
	e.Ret(checkHalves(f, e, e.Add(ir.ClsL, buf, f.Long(384))))
	require.Equal(t, 0, runObj(t, m))
}

// TestObjWideIndexedAccess exercises the folded [alloca + index*scale] form at run
// time. The index comes back from a called function so nothing can constant-fold
// it, and the two accesses use different indices, so a scale or index that is
// silently ignored moves the wrong 16 bytes rather than merely the wrong number of
// them.
func TestObjWideIndexedAccess(t *testing.T) {
	m := ir.NewModule()
	idf := m.NewFunc("wideidentity", ir.ClsL)
	x := idf.Param("x", ir.ClsL)
	idf.Entry().Ret(x)

	f, e := entry(m)
	buf := e.Alloc(16, 128)
	two := e.Call(ir.ClsL, f.Sym("wideidentity", 0), f.Long(2))
	five := e.Call(ir.ClsL, f.Sym("wideidentity", 0), f.Long(5))
	// buf[two*8] gets the value; buf[five*8] receives the copy.
	fillWide(f, e, e.Add(ir.ClsL, buf, e.Shl(ir.ClsL, two, f.Long(3))))
	e.Store(e.Load(ir.ClsQ, e.Add(ir.ClsL, buf, e.Shl(ir.ClsL, two, f.Long(3)))),
		e.Add(ir.ClsL, buf, e.Shl(ir.ClsL, five, f.Long(3))))
	e.Ret(checkHalves(f, e, e.Add(ir.ClsL, buf, e.Shl(ir.ClsL, five, f.Long(3)))))
	require.Equal(t, 0, runObj(t, m))
}

// TestObjWideGlobalAccess runs the RIP-relative form: a 128-bit copy between two
// globals, which is the one addressing mode whose displacement is written by a
// relocation rather than by the emitter.
func TestObjWideGlobalAccess(t *testing.T) {
	m := ir.NewModule()
	m.Data = append(m.Data, wideZeroData("wsrc"), wideZeroData("wdst"))
	f, e := entry(m)
	fillWide(f, e, f.Sym("wsrc", 0))
	e.Store(e.Load(ir.ClsQ, f.Sym("wsrc", 0)), f.Sym("wdst", 0))
	e.Ret(checkHalves(f, e, f.Sym("wdst", 0)))
	require.Equal(t, 0, runObj(t, m))
}

// TestObjWideByteArrayMove is the [16]byte shape: sixteen individually written
// bytes moved as one 128-bit unit, with every byte checked at the destination. A
// half-width move leaves the second eight bytes zero, which the per-byte sum
// catches; a byte-swapped or rotated move changes the weighted sum.
func TestObjWideByteArrayMove(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	src := e.Alloc(16, 16)
	dst := e.Alloc(16, 16)
	for i := 0; i < 16; i++ {
		e.StoreSub(ir.SubB, f.Word(int64(0xA0+i)), e.Add(ir.ClsL, src, f.Long(int64(i))))
	}
	e.Store(e.Load(ir.ClsQ, src), dst)
	// Sum (i+1)*byte[i], which distinguishes any reordering as well as any
	// truncation, and compare with the expected total.
	want := 0
	acc := f.Word(0)
	for i := 0; i < 16; i++ {
		want += (i + 1) * (0xA0 + i)
		b := e.LoadSub(ir.ClsW, ir.SubUB, e.Add(ir.ClsL, dst, f.Long(int64(i))))
		acc = e.Add(ir.ClsW, acc, e.Mul(ir.ClsW, b, f.Word(int64(i+1))))
	}
	e.Ret(e.Cmp(ir.CmpNe, ir.ClsW, acc, f.Word(int64(want))))
	require.Equal(t, 0, runObj(t, m))
}

// TestObjWideComplex128Move is the complex128 shape: a {re, im} pair of doubles
// moved as one 128-bit value, then read back as doubles and compared. Written
// through the float path on both sides, so nothing but the 128-bit move sees the
// value as a single unit.
func TestObjWideComplex128Move(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	src := e.Alloc(16, 16)
	dst := e.Alloc(16, 16)
	e.Store(f.Double(-2.5), src)                             // real part
	e.Store(f.Double(1e300), e.Add(ir.ClsL, src, f.Long(8))) // imaginary part
	e.Store(e.Load(ir.ClsQ, src), dst)
	re := e.Load(ir.ClsD, dst)
	im := e.Load(ir.ClsD, e.Add(ir.ClsL, dst, f.Long(8)))
	badRe := e.Cmp(ir.CmpFne, ir.ClsW, re, f.Double(-2.5))
	badIm := e.Cmp(ir.CmpFne, ir.ClsW, im, f.Double(1e300))
	e.Ret(e.Add(ir.ClsW, badRe, e.Mul(ir.ClsW, badIm, f.Word(2))))
	require.Equal(t, 0, runObj(t, m))
}

// TestObjWidePhiRoundTrip runs the ir.OCopy path: which 128-bit value reaches the
// store is decided at run time, so SSA destruction has to move one, and it has to
// move all sixteen bytes. Both branches are taken, in two calls.
func TestObjWidePhiRoundTrip(t *testing.T) {
	m := ir.NewModule()

	// pick(sel) copies buf[sel != 0 ? 0 : 16] to buf[32] and reports whether the
	// halves at buf[32] are the ones it selected.
	g := m.NewFunc("widepick", ir.ClsW)
	sel := g.Param("sel", ir.ClsW)
	ge := g.Entry()
	left := g.NewBlock("left")
	right := g.NewBlock("right")
	join := g.NewBlock("join")

	buf := ge.Alloc(16, 64)
	fillWide(g, ge, buf)                                  // the wanted value at +0
	ge.Store(g.Long(0), ge.Add(ir.ClsL, buf, g.Long(16))) // zeroes at +16
	ge.Store(g.Long(0), ge.Add(ir.ClsL, buf, g.Long(24))) //
	ge.Jnz(sel, left, right)
	a := left.Load(ir.ClsQ, buf)
	left.Goto(join)
	b := right.Load(ir.ClsQ, right.Add(ir.ClsL, buf, g.Long(16)))
	right.Goto(join)
	v := join.Phi(ir.ClsQ, ir.PhiEdge{From: left, Val: a}, ir.PhiEdge{From: right, Val: b})
	join.Store(v, join.Add(ir.ClsL, buf, g.Long(32)))
	// sel != 0 wants the filled value; sel == 0 wants both halves zero.
	lo := join.Load(ir.ClsL, join.Add(ir.ClsL, buf, g.Long(32)))
	hi := join.Load(ir.ClsL, join.Add(ir.ClsL, buf, g.Long(40)))
	join.Ret(join.Add(ir.ClsW,
		join.Cmp(ir.CmpNe, ir.ClsW, lo, g.Long(wideLo)),
		join.Mul(ir.ClsW, join.Cmp(ir.CmpNe, ir.ClsW, hi, g.Long(wideHi)), g.Word(2))))

	f, e := entry(m)
	// widepick(1) must report 0 (it selected the filled value); widepick(0) must
	// report 3 (both halves differ, because it selected the zeroed one).
	one := e.Call(ir.ClsW, f.Sym("widepick", 0), f.Word(1))
	zero := e.Call(ir.ClsW, f.Sym("widepick", 0), f.Word(0))
	e.Ret(e.Add(ir.ClsW, one, e.Cmp(ir.CmpNe, ir.ClsW, zero, f.Word(3))))
	require.Equal(t, 0, runObj(t, m))
}

// TestObjWideManyValues raises XMM pressure with several live 128-bit values at
// once, so the copies between them and the accesses that reload them are not all
// trivially assigned the same register. Six live values still fit the fourteen
// allocatable XMMs, which is the point: nothing here should hit the spill refusal.
func TestObjWideManyValues(t *testing.T) {
	const n = 6
	m := ir.NewModule()
	f, e := entry(m)
	buf := e.Alloc(16, 16*(2*n+1))
	var vals []ir.Ref
	for i := 0; i < n; i++ {
		at := e.Add(ir.ClsL, buf, f.Long(int64(16*i)))
		e.Store(f.Long(wideLo+int64(i)), at)
		e.Store(f.Long(wideHi+int64(i)), e.Add(ir.ClsL, buf, f.Long(int64(16*i+8))))
		vals = append(vals, e.Load(ir.ClsQ, e.Add(ir.ClsL, buf, f.Long(int64(16*i)))))
	}
	// Store them back in reverse, so every one of them is live until the last.
	for i, v := range vals {
		e.Store(v, e.Add(ir.ClsL, buf, f.Long(int64(16*(2*n-i)))))
	}
	acc := f.Word(0)
	for i := range vals {
		at := int64(16 * (2*n - i))
		lo := e.Load(ir.ClsL, e.Add(ir.ClsL, buf, f.Long(at)))
		hi := e.Load(ir.ClsL, e.Add(ir.ClsL, buf, f.Long(at+8)))
		acc = e.Add(ir.ClsW, acc, e.Cmp(ir.CmpNe, ir.ClsW, lo, f.Long(wideLo+int64(i))))
		acc = e.Add(ir.ClsW, acc, e.Cmp(ir.CmpNe, ir.ClsW, hi, f.Long(wideHi+int64(i))))
	}
	e.Ret(acc)
	require.Equal(t, 0, runObj(t, m))
}
