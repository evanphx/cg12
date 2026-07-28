package amd64_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// This file covers the register allocation of a folded [base + index*scale + disp]
// operand, in the case where the base has no register of its own.
//
// memFor has exactly one general-purpose scratch register, gpScratch1: a store's
// value is already resolved into gpScratch0 by the time the address is built, and a
// spilled load destination is gpScratch0 as well. It used to spend that one register
// twice when both the base and the index needed loading -- the base first, then the
// index over the top of it -- and emitted `mov (%r11,%r11,4), %esi`, reading
// [index + index*scale] from an address that has nothing to do with the object. With
// an index of 0 that dereferences 0 and the process takes SIGSEGV.
//
// The shape that reaches it is ordinary: a stack array whose address is passed to a
// call, which is what stops the address being rematerialised (remat.go's
// srcResolvesOperands), under enough pressure to spill it -- plus a runtime index
// that is spilled too. No unusual types are involved; the accesses below are 32-bit
// integer loads and 64-bit stores.

// spillPressure is how many base/index pairs the modules below keep live at once.
// amd64 has nine allocatable GP registers (intAllocOrder), so twelve live pointers
// plus twelve live indices leaves several of both in spill slots.
const spillPressure = 12

// poisonWords is how many 4-byte words of each buffer are pre-filled with a value
// no access under test should return. It makes a wrong address inside the buffer --
// a dropped index, a wrong scale, the wrong buffer -- a deterministic wrong answer
// rather than whatever the stack happened to hold.
const poisonWords = 16

const (
	bufBytes = poisonWords * 4
	poison   = -1000
)

// indexOf is the runtime index used for buffer i: never 0, so an index that is
// silently dropped reads the poisoned word 0 instead of quietly getting away with
// it, and small enough that indexOf(i)*8 stays inside the buffer.
func indexOf(i int) int64 { return int64(1 + i%3) }

// pressureModule builds `runtest` out of spillPressure buffers, each accessed once
// through a folded [buffer + index*scale] operand, and returns 0 exactly when every
// access read or wrote the word its index names.
//
// Three things force the operands the test is about:
//   - each buffer's address is passed to `sink`, so it is not rematerialisable and
//     has to live in a register or a spill slot;
//   - every index comes back from `ident`, so it is a runtime value, and all of them
//     are live at once, so the allocator spills some;
//   - for the store direction the stored values come back from `ident` too, so on
//     top of a spilled base and a spilled index the value occupies gpScratch0.
func pressureModule(scale int64, store bool) *ir.Module {
	m := ir.NewModule()

	// sink takes a pointer and ignores it. Passing an alloca to a call is what makes
	// its address non-rematerialisable, which is what lets it end up in a spill slot.
	sink := m.NewFunc("sink", ir.ClsW)
	sink.Param("p", ir.ClsL)
	sink.Entry().Ret(sink.Word(0))

	// ident returns its argument, so a value that came from it cannot be constant
	// folded back into the access.
	id := m.NewFunc("ident", ir.ClsL)
	x := id.Param("x", ir.ClsL)
	id.Entry().Ret(x)

	f, e := entry(m)

	var bufs []ir.Ref
	for i := 0; i < spillPressure; i++ {
		buf := e.Alloc(8, bufBytes)
		e.CallVoid(f.Sym("sink", 0), buf)
		bufs = append(bufs, buf)
	}
	for _, buf := range bufs {
		for w := 0; w < poisonWords; w++ {
			e.StoreSub(ir.SubW, f.Word(poison), e.Add(ir.ClsL, buf, f.Long(int64(4*w))))
		}
	}

	var idxs []ir.Ref
	for i := 0; i < spillPressure; i++ {
		idxs = append(idxs, e.Call(ir.ClsL, f.Sym("ident", 0), f.Long(indexOf(i))))
	}
	var vals []ir.Ref
	if store {
		for i := 0; i < spillPressure; i++ {
			vals = append(vals, e.Call(ir.ClsL, f.Sym("ident", 0), f.Long(int64(i+1))))
		}
	}

	// scaled builds the folded address: the shift the fold turns into the scale is
	// omitted for scale 1, which needs no shift to fold.
	scaled := func(buf, idx ir.Ref) ir.Ref {
		if scale == 1 {
			return e.Add(ir.ClsL, buf, idx)
		}
		return e.Add(ir.ClsL, buf, e.Shl(ir.ClsL, idx, f.Long(shiftFor(scale))))
	}

	want := 0
	acc := f.Word(0)
	for i := 0; i < spillPressure; i++ {
		at := indexOf(i) * scale
		if store {
			// The store goes through the folded operand; the check reads it back
			// through a plain [base + disp] one, which needs no index.
			e.Store(vals[i], scaled(bufs[i], idxs[i]))
			got := e.Load(ir.ClsW, e.Add(ir.ClsL, bufs[i], f.Long(at)))
			acc = e.Add(ir.ClsW, acc, got)
		} else {
			// Mirrored: a plain store puts the sentinel where the index names, and the
			// folded operand is the one that has to find it.
			e.StoreSub(ir.SubW, f.Word(int64(i+1)), e.Add(ir.ClsL, bufs[i], f.Long(at)))
			acc = e.Add(ir.ClsW, acc, e.Load(ir.ClsW, scaled(bufs[i], idxs[i])))
		}
		want += i + 1
	}
	e.Ret(e.Sub(ir.ClsW, acc, f.Word(int64(want))))
	return m
}

// shiftFor is the shift amount that folds into an index scale of 2, 4 or 8.
func shiftFor(scale int64) int64 {
	switch scale {
	case 2:
		return 1
	case 4:
		return 2
	case 8:
		return 3
	}
	panic(fmt.Sprintf("scale %d does not fold", scale))
}

// foldScales are the index scales a SIB byte can express, all of which the fold
// produces.
var foldScales = []int64{1, 2, 4, 8}

// TestObjSpilledBaseIndexedLoad is the regression test: before the fix each of these
// dies with SIGSEGV (runObj reports -1), because the base register is overwritten by
// the index and the access dereferences a small integer.
func TestObjSpilledBaseIndexedLoad(t *testing.T) {
	for _, scale := range foldScales {
		t.Run(fmt.Sprintf("scale%d", scale), func(t *testing.T) {
			require.Equal(t, 0, runObj(t, pressureModule(scale, false)))
		})
	}
}

// TestObjSpilledBaseIndexedStore is the same on the store side, which is also the
// case that rules out giving the index gpScratch0 instead: the stored value is
// already there. All three of base, index and value need a scratch register at the
// same point here, and there are two.
func TestObjSpilledBaseIndexedStore(t *testing.T) {
	for _, scale := range foldScales {
		t.Run(fmt.Sprintf("scale%d", scale), func(t *testing.T) {
			require.Equal(t, 0, runObj(t, pressureModule(scale, true)))
		})
	}
}

// widePressureModule is pressureModule for the 128-bit pair (ir.OLoadq/ir.OStoreq),
// the third family whose address memFor resolves. Both ends of the copy are folded
// indexed operands over a spilled base: the load reads sixteen bytes from
// [buffer + src*8] and the store writes them to [buffer + dst*8], with the two index
// ranges chosen so the copy never overlaps itself.
func widePressureModule() *ir.Module {
	m := ir.NewModule()

	sink := m.NewFunc("sink", ir.ClsW)
	sink.Param("p", ir.ClsL)
	sink.Entry().Ret(sink.Word(0))

	id := m.NewFunc("ident", ir.ClsL)
	x := id.Param("x", ir.ClsL)
	id.Entry().Ret(x)

	f, e := entry(m)

	var bufs []ir.Ref
	for i := 0; i < spillPressure; i++ {
		buf := e.Alloc(16, bufBytes)
		e.CallVoid(f.Sym("sink", 0), buf)
		bufs = append(bufs, buf)
	}
	// Source elements live in the low half of each buffer, destinations in the high
	// half: 8*srcIdx + 16 <= 40 <= 8*dstIdx.
	srcIdx := func(i int) int64 { return int64(1 + i%3) }
	dstIdx := func(i int) int64 { return int64(5 + i%2) }

	var srcs, dsts []ir.Ref
	for i := 0; i < spillPressure; i++ {
		srcs = append(srcs, e.Call(ir.ClsL, f.Sym("ident", 0), f.Long(srcIdx(i))))
	}
	for i := 0; i < spillPressure; i++ {
		dsts = append(dsts, e.Call(ir.ClsL, f.Sym("ident", 0), f.Long(dstIdx(i))))
	}

	acc := f.Word(0)
	for i, buf := range bufs {
		fillWide(f, e, e.Add(ir.ClsL, buf, f.Long(8*srcIdx(i))))
		from := e.Add(ir.ClsL, buf, e.Shl(ir.ClsL, srcs[i], f.Long(3)))
		to := e.Add(ir.ClsL, buf, e.Shl(ir.ClsL, dsts[i], f.Long(3)))
		e.Store(e.Load(ir.ClsQ, from), to)
		acc = e.Add(ir.ClsW, acc, checkHalves(f, e, e.Add(ir.ClsL, buf, f.Long(8*dstIdx(i)))))
	}
	e.Ret(acc)
	return m
}

// TestObjSpilledBaseIndexedWideAccess runs the collision through the 128-bit pair. A
// 128-bit value has to live in an XMM register, so nothing contends for gpScratch0
// here; what this adds is that the address the collapsed operand names is still the
// right one for an access whose value is not a GP register at all, on both the load
// and the store side.
func TestObjSpilledBaseIndexedWideAccess(t *testing.T) {
	require.Equal(t, 0, runObj(t, widePressureModule()))
}

// --- encoding layer --------------------------------------------------------

// memOperandRe matches a disassembled memory operand: an optional displacement, a
// base register, and optionally an index register and its scale.
var memOperandRe = regexp.MustCompile(`(-?(?:0x)?[0-9a-f]*)\(%([a-z][a-z0-9]*)(?:,%([a-z][a-z0-9]*)(?:,([1248]))?)?\)`)

// requireNoSelfIndexedOperand is the assertion that keeps the bug from coming back:
// no memory operand may name one register as both its base and its index. That is
// what `mov (%r11,%r11,4), %esi` is, and it can only arise from the base and the
// index having been resolved into the same scratch register -- a base+index*scale
// operand where both halves are the same value is never a correct address for a
// distinct base and index.
func requireNoSelfIndexedOperand(t *testing.T, instrs []string) {
	t.Helper()
	for _, in := range instrs {
		for _, mo := range memOperandRe.FindAllStringSubmatch(in, -1) {
			base, index := mo[2], mo[3]
			if index == "" {
				continue
			}
			require.NotEqualf(t, base, index,
				"%q addresses [index + index*scale]: the base was overwritten by the index, in:\n\t%s",
				in, strings.Join(instrs, "\n\t"))
		}
	}
}

// requireStoreValueOutsideItsAddress is the assertion for the other way to run out
// of registers here: handing the index gpScratch0 would work for a load but would
// overwrite a spilled store's value, which is already sitting in gpScratch0 when the
// address is built. That shows up as the stored register appearing inside its own
// memory operand.
//
// In these modules the stored value is never derived from the address it is stored
// to (it comes from a separate `ident` call), so a store that names one register as
// both its value and part of its address has lost one of the two.
func requireStoreValueOutsideItsAddress(t *testing.T, instrs []string) {
	t.Helper()
	// A store's operands are "%reg, mem": a register source, then a memory
	// destination.
	storeRe := regexp.MustCompile(`^mov[a-z]* %([a-z][a-z0-9]*), (.*\(%.*\))$`)
	for _, in := range instrs {
		mv := storeRe.FindStringSubmatch(in)
		if mv == nil {
			continue
		}
		val := normalizeReg(mv[1])
		for _, mo := range memOperandRe.FindAllStringSubmatch(mv[2], -1) {
			require.NotEqualf(t, val, normalizeReg(mo[2]),
				"%q stores a register that is part of its own address, in:\n\t%s",
				in, strings.Join(instrs, "\n\t"))
			if mo[3] != "" {
				require.NotEqualf(t, val, normalizeReg(mo[3]),
					"%q indexes by the register it stores, in:\n\t%s",
					in, strings.Join(instrs, "\n\t"))
			}
		}
	}
}

// normalizeReg maps a register name to its 64-bit form, so a value held in %r10d and
// an address using %r10 compare equal -- they are the same register, and clobbering
// one clobbers the other.
func normalizeReg(name string) string {
	switch name {
	case "eax", "ax", "al":
		return "rax"
	case "ecx", "cx", "cl":
		return "rcx"
	case "edx", "dx", "dl":
		return "rdx"
	case "ebx", "bx", "bl":
		return "rbx"
	case "esi", "si", "sil":
		return "rsi"
	case "edi", "di", "dil":
		return "rdi"
	case "esp", "sp":
		return "rsp"
	case "ebp", "bp":
		return "rbp"
	}
	// r8..r15 in their d/w/b forms.
	if strings.HasPrefix(name, "r") {
		return strings.TrimRight(name, "dwb")
	}
	return name
}

// requireInstrMatching is requireInstrLike for a case that needs a real pattern: it
// asserts some instruction matches the regular expression.
func requireInstrMatching(t *testing.T, instrs []string, pattern string) {
	t.Helper()
	re := regexp.MustCompile(pattern)
	for _, in := range instrs {
		if re.MatchString(in) {
			return
		}
	}
	t.Fatalf("expected an instruction matching %q among:\n\t%s", pattern, strings.Join(instrs, "\n\t"))
}

// TestFoldedIndexedOperandNeverReusesOneRegisterTwice is the encoding half of the
// regression: whatever the allocator did with the base, the index and (for a store)
// the value, the emitted operands never spend one register on two of them.
func TestFoldedIndexedOperandNeverReusesOneRegisterTwice(t *testing.T) {
	for _, scale := range foldScales {
		for _, store := range []bool{false, true} {
			name := fmt.Sprintf("scale%d/load", scale)
			if store {
				name = fmt.Sprintf("scale%d/store", scale)
			}
			t.Run(name, func(t *testing.T) {
				instrs := wideAsm(t, pressureModule(scale, store), "runtest")
				requireNoSelfIndexedOperand(t, instrs)
				requireStoreValueOutsideItsAddress(t, instrs)
			})
		}
	}
	t.Run("wide", func(t *testing.T) {
		instrs := wideAsm(t, widePressureModule(), "runtest")
		requireNoSelfIndexedOperand(t, instrs)
		requireStoreValueOutsideItsAddress(t, instrs)
	})
}

// TestSpilledBaseIndexedOperandCollapsesIntoOneRegister pins how the operand is
// built when there is only one register for two operands: the address is computed
// into that register -- index loaded, scaled by a shift, base added straight from
// its spill slot -- and the access then uses it as a plain base.
//
// Without this the test above could pass for the wrong reason (an operand with no
// index at all cannot alias its base), so it states that the address really is
// base + index*scale and that it was built in one register.
func TestSpilledBaseIndexedOperandCollapsesIntoOneRegister(t *testing.T) {
	instrs := wideAsm(t, pressureModule(4, true), "runtest")
	requireInstrMatching(t, instrs, `^movq -?[0-9a-fx]+\(%rbp\), %r11$`) // an operand from its slot
	requireInstrMatching(t, instrs, `^shlq \$2, %r11$`)                  // the index, scaled by 4
	requireInstrMatching(t, instrs, `^addq -?[0-9a-fx]+\(%rbp\), %r11$`) // plus the spilled base
	requireInstrLike(t, instrs, "movq %g, (%r11)")                       // and the access itself
}

// TestUnspilledBaseIndexedOperandStaysASIB is the other side of the same coin: when
// the base does have a register, nothing changes and the whole address is still one
// SIB operand. This is what says the fix did not buy correctness by giving up the
// fold; the module is the same shape as above with only one buffer, so nothing
// spills.
func TestUnspilledBaseIndexedOperandStaysASIB(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("oneidx", ir.ClsW).Export()
	i := f.Param("i", ir.ClsL)
	e := f.Entry()
	buf := e.Alloc(8, bufBytes)
	e.Ret(e.Load(ir.ClsW, e.Add(ir.ClsL, buf, e.Shl(ir.ClsL, i, f.Long(2)))))

	instrs := wideAsm(t, m, "oneidx")
	requireInstrLike(t, instrs, "movl (%g,%g,4), %g")
	requireNoSelfIndexedOperand(t, instrs)
	// The address arithmetic was folded, not computed: no shift and no add survive.
	requireNoInstrPrefix(t, instrs, "shlq")
	requireNoInstrPrefix(t, instrs, "addq")
}

// TestFoldedIndexWithATwoRegisterBaseIsRefused covers the one (base, index) pairing
// memFor cannot encode out of its single scratch register: a base that is neither in
// a register of its own nor in a memory home it can be added from. Here it is a
// literal address, which has to be materialised into a register -- and with the
// index needing the scratch as well, there is no second one to materialise it into.
//
// foldAddressing never produces this: it pairs an index only with an alloca base,
// whose home is a register, a rematerialised frame address, or a spill slot, and a
// literal base only ever reaches memFor in the index-free [base + disp] shape. So
// the operand is built by hand -- the fold leaves an instruction that already has an
// Amode alone -- and what the test pins is that the emitter says so rather than
// quietly encoding an address with the base missing.
func TestFoldedIndexWithATwoRegisterBaseIsRefused(t *testing.T) {
	m := ir.NewModule()
	f, e := entry(m)
	e.Ret(e.Load(ir.ClsW, f.Long(0x2000)))

	// Rewrite the load's address into the folded indexed form: [0x2000 + 2*4].
	load := &e.Instrs[len(e.Instrs)-1]
	require.Truef(t, load.Op.IsLoad(), "the last instruction should be the load, got %s", load.Op)
	load.Args = []ir.Ref{f.Long(0x2000), f.Long(2)}
	load.Amode = 4

	_, err := amd64.CompileObject(m)
	require.Error(t, err)
	require.Contains(t, err.Error(), "two scratch registers")
}
