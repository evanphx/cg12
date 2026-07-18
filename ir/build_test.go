package ir

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildAndPrintAdd(t *testing.T) {
	m := NewModule()
	f := m.NewFunc("add", ClsW).Export()
	a := f.Param("a", ClsW)
	b := f.Param("b", ClsW)
	e := f.Entry()
	e.Ret(e.Add(ClsW, a, b))

	want := `export function w $add(w %a, w %b) {
@start
	%t1 =w add %a, %b
	ret %t1
}
`
	assert.Equal(t, want, f.String())
}

func TestEntryCreatesStart(t *testing.T) {
	f := NewModule().NewFuncVoid("v")
	e := f.Entry()
	assert.NotNil(t, e)
	assert.Same(t, e, f.Start)
	// Calling Entry again returns the same block.
	assert.Same(t, e, f.Entry())
	e.RetVoid()
	assert.Equal(t, "function $v() {\n@start\n\tret\n}\n", f.String())
}

func TestNewBlockAutoNames(t *testing.T) {
	f := NewModule().NewFuncVoid("v")
	b1 := f.NewBlock("")
	b2 := f.NewBlock("")
	assert.Equal(t, "b1", b1.Name)
	assert.Equal(t, "b2", b2.Name)
	assert.Same(t, b1, f.Start)
}

func TestArithmeticOps(t *testing.T) {
	f := NewModule().NewFunc("f", ClsL)
	x := f.Param("x", ClsL)
	y := f.Param("y", ClsL)
	e := f.Entry()

	cases := []struct {
		got Ref
		op  Op
	}{
		{e.Add(ClsL, x, y), OAdd},
		{e.Sub(ClsL, x, y), OSub},
		{e.Mul(ClsL, x, y), OMul},
		{e.Div(ClsL, x, y), ODiv},
		{e.UDiv(ClsL, x, y), OUDiv},
		{e.Rem(ClsL, x, y), ORem},
		{e.URem(ClsL, x, y), OURem},
		{e.And(ClsL, x, y), OAnd},
		{e.Or(ClsL, x, y), OOr},
		{e.Xor(ClsL, x, y), OXor},
		{e.Shl(ClsL, x, y), OShl},
		{e.Shr(ClsL, x, y), OShr},
		{e.Sar(ClsL, x, y), OSar},
		{e.Neg(ClsL, x), ONeg},
		{e.Copy(ClsL, x), OCopy},
		{e.Cast(ClsD, x), OCast},
	}
	require.Len(t, e.Instrs, len(cases))
	for i, c := range cases {
		in := e.Instrs[i]
		assert.Equal(t, c.op, in.Op, in.Op.String())
		assert.True(t, c.got.IsTemp())
		assert.Equal(t, in.To, c.got)
	}
	// Result classes track the requested class.
	assert.Equal(t, ClsD, f.ClassOf(cases[len(cases)-1].got))
}

func TestCallerFrameIntrinsics(t *testing.T) {
	function := NewModule().NewFunc("caller", ClsL)
	entry := function.Entry()
	callerPC := entry.CallerPC()
	callerSP := entry.CallerSP()
	entry.Ret(callerPC)

	require.Len(t, entry.Instrs, 2)
	assert.Equal(t, OIntrinsic, entry.Instrs[0].Op)
	assert.Equal(t, "getcallerpc", entry.Instrs[0].Intrin.Name)
	assert.Equal(t, ClsL, function.ClassOf(callerPC))
	assert.Equal(t, OIntrinsic, entry.Instrs[1].Op)
	assert.Equal(t, "getcallersp", entry.Instrs[1].Intrin.Name)
	assert.Equal(t, ClsL, function.ClassOf(callerSP))
}

func TestCompareMnemonic(t *testing.T) {
	f := NewModule().NewFunc("lt", ClsW)
	a := f.Param("a", ClsW)
	b := f.Param("b", ClsW)
	e := f.Entry()
	e.Ret(e.Cmp(CmpSlt, ClsW, a, b))

	want := `function w $lt(w %a, w %b) {
@start
	%t1 =w csltw %a, %b
	ret %t1
}
`
	assert.Equal(t, want, f.String())
}

func TestFloatCompareMnemonic(t *testing.T) {
	f := NewModule().NewFunc("flt", ClsW)
	a := f.Param("a", ClsD)
	b := f.Param("b", ClsD)
	e := f.Entry()
	// Result is a word; operands are doubles -> "cltd".
	r := e.Cmp(CmpFlt, ClsW, a, b)
	e.Ret(r)
	assert.Contains(t, f.String(), "%t1 =w cltd %a, %b")
}

func TestLoadStoreOpMapping(t *testing.T) {
	f := NewModule().NewFuncVoid("mem")
	p := f.Param("p", ClsL)
	e := f.Entry()

	assert.Equal(t, OLoaduw, opOf(e, func() { e.Load(ClsW, p) }))
	assert.Equal(t, OLoadl, opOf(e, func() { e.Load(ClsL, p) }))
	assert.Equal(t, OLoads, opOf(e, func() { e.Load(ClsS, p) }))
	assert.Equal(t, OLoadd, opOf(e, func() { e.Load(ClsD, p) }))

	assert.Equal(t, OLoadsb, opOf(e, func() { e.LoadSub(ClsW, SubB, p) }))
	assert.Equal(t, OLoadub, opOf(e, func() { e.LoadSub(ClsW, SubUB, p) }))
	assert.Equal(t, OLoadsh, opOf(e, func() { e.LoadSub(ClsW, SubH, p) }))
	assert.Equal(t, OLoaduh, opOf(e, func() { e.LoadSub(ClsW, SubUH, p) }))
	assert.Equal(t, OLoadsw, opOf(e, func() { e.LoadSub(ClsL, SubW, p) }))

	w := f.Word(7)
	l := f.Long(7)
	s := f.Single(1.5)
	d := f.Double(2.5)
	assert.Equal(t, OStorew, opOf(e, func() { e.Store(w, p) }))
	assert.Equal(t, OStorel, opOf(e, func() { e.Store(l, p) }))
	assert.Equal(t, OStores, opOf(e, func() { e.Store(s, p) }))
	assert.Equal(t, OStored, opOf(e, func() { e.Store(d, p) }))
	assert.Equal(t, OStoreb, opOf(e, func() { e.StoreSub(SubB, w, p) }))
	assert.Equal(t, OStoreh, opOf(e, func() { e.StoreSub(SubH, w, p) }))
}

// opOf runs emit and returns the op of the instruction it appended.
func opOf(b *Block, emit func()) Op {
	emit()
	return b.Instrs[len(b.Instrs)-1].Op
}

func TestAlloc(t *testing.T) {
	f := NewModule().NewFuncVoid("a")
	e := f.Entry()
	r4 := e.Alloc(4, 16)
	r8 := e.Alloc(8, 32)
	r16 := e.Alloc(16, 64)
	assert.Equal(t, OAlloc4, e.Instrs[0].Op)
	assert.Equal(t, OAlloc8, e.Instrs[1].Op)
	assert.Equal(t, OAlloc16, e.Instrs[2].Op)
	// Allocs yield the abstract pointer class, resolved per target later.
	assert.Equal(t, ClsP, f.ClassOf(r4))
	assert.Equal(t, ClsP, f.ClassOf(r8))
	assert.Equal(t, ClsP, f.ClassOf(r16))
	// Size stored as a long constant operand.
	sz := e.Instrs[0].Args[0]
	assert.Equal(t, int64(16), f.Consts[sz.ID].Int)
}

func TestAllocBadAlignmentPanics(t *testing.T) {
	f := NewModule().NewFuncVoid("a")
	e := f.Entry()
	assert.Panics(t, func() { e.Alloc(3, 8) })
}

func TestCallPrinting(t *testing.T) {
	f := NewModule().NewFunc("g", ClsW)
	e := f.Entry()
	r := e.Call(ClsW, f.Sym("add", 0), f.Word(2), f.Word(3))
	e.Ret(r)
	want := `function w $g() {
@start
	%t1 =w call $add(w 2, w 3)
	ret %t1
}
`
	assert.Equal(t, want, f.String())
}

func TestCallVoid(t *testing.T) {
	f := NewModule().NewFuncVoid("g")
	e := f.Entry()
	e.CallVoid(f.Sym("puts", 0), f.Sym("msg", 0))
	e.RetVoid()
	s := f.String()
	assert.Contains(t, s, "\tcall $puts(p $msg)\n") // symbol addresses are pointer-class
	// A void call defines no result temp.
	assert.True(t, e.Instrs[0].To.IsNone())
}

func TestPhiAndJnz(t *testing.T) {
	f := NewModule().NewFunc("f", ClsW)
	n := f.Param("n", ClsW)
	start := f.Entry()
	ba := f.NewBlock("a")
	bb := f.NewBlock("b")
	bc := f.NewBlock("c")

	start.Jnz(n, ba, bb)
	ba.Goto(bc)
	bb.Goto(bc)
	r := bc.Phi(ClsW, PhiEdge{ba, f.Word(1)}, PhiEdge{bb, f.Word(2)})
	bc.Ret(r)

	want := `function w $f(w %n) {
@start
	jnz %n, @a, @b
@a
	jmp @c
@b
	jmp @c
@c
	%t1 =w phi @a 1, @b 2
	ret %t1
}
`
	assert.Equal(t, want, f.String())
}

func TestPhiAddEdge(t *testing.T) {
	f := NewModule().NewFuncVoid("loop")
	start := f.Entry()
	loop := f.NewBlock("loop")
	start.Goto(loop)

	// Back-edge value not known when the phi is created.
	iref := loop.Phi(ClsW, PhiEdge{start, f.Word(0)})
	next := loop.Add(ClsW, iref, f.Word(1))
	loop.Phis[0].Add(loop, next)
	loop.Goto(loop)

	assert.Contains(t, f.String(), "phi @start 0, @loop %t2")
}

func TestConstInterning(t *testing.T) {
	f := NewModule().NewFuncVoid("c")
	a := f.Word(5)
	b := f.Word(5)
	c := f.Long(5)
	assert.Equal(t, a, b, "equal word constants must share a slot")
	assert.NotEqual(t, a, c, "different classes must not share a slot")
	assert.Len(t, f.Consts, 2)

	// Symbols and floats intern independently.
	s1 := f.Sym("x", 0)
	s2 := f.Sym("x", 0)
	s3 := f.Sym("x", 8)
	assert.Equal(t, s1, s2)
	assert.NotEqual(t, s1, s3)

	d1 := f.Double(1.5)
	d2 := f.Double(1.5)
	assert.Equal(t, d1, d2)
}

func TestConstString(t *testing.T) {
	f := NewModule().NewFuncVoid("c")
	assert.Equal(t, "5", f.refString(f.Word(5)))
	assert.Equal(t, "-3", f.refString(f.Long(-3)))
	assert.Equal(t, "s_1.5", f.refString(f.Single(1.5)))
	assert.Equal(t, "d_2.5", f.refString(f.Double(2.5)))
	assert.Equal(t, "$x", f.refString(f.Sym("x", 0)))
	assert.Equal(t, "$x+8", f.refString(f.Sym("x", 8)))
}

func TestConversionBuilders(t *testing.T) {
	f := NewModule().NewFuncVoid("c")
	w := f.Param("w", ClsW)
	l := f.Param("l", ClsL)
	s := f.Param("s", ClsS)
	d := f.Param("d", ClsD)
	e := f.Entry()

	cases := []struct {
		got Ref
		op  Op
		cls Cls
	}{
		{e.Extsb(ClsW, w), OExtsb, ClsW},
		{e.Extub(ClsW, w), OExtub, ClsW},
		{e.Extsh(ClsW, w), OExtsh, ClsW},
		{e.Extuh(ClsW, w), OExtuh, ClsW},
		{e.Extsw(ClsL, w), OExtsw, ClsL},
		{e.Extuw(ClsL, w), OExtuw, ClsL},
		{e.Exts(s), OExts, ClsD},
		{e.Truncd(d), OTruncd, ClsS},
		{e.Stosi(ClsL, d), OStosi, ClsL},
		{e.Stoui(ClsL, d), OStoui, ClsL},
		{e.Sltof(ClsD, l), OSltof, ClsD},
		{e.Ultof(ClsD, l), OUltof, ClsD},
	}
	require.Len(t, e.Instrs, len(cases))
	for i, c := range cases {
		assert.Equal(t, c.op, e.Instrs[i].Op, c.op.String())
		assert.Equal(t, c.cls, f.ClassOf(c.got), c.op.String())
	}
}

func TestNewTempAndRef(t *testing.T) {
	f := NewModule().NewFuncVoid("x")
	r := f.NewTemp("v", ClsL)
	assert.True(t, r.IsTemp())
	tp := f.Temp(r)
	assert.Equal(t, "v", tp.Name)
	assert.Equal(t, ClsL, tp.Cls)
	assert.Equal(t, NoReg, tp.Reg)
	assert.Equal(t, -1, tp.Slot)
	// Temp.Ref round-trips back to the same reference.
	assert.Equal(t, r, tp.Ref())
}

func TestClassOfAndTemp(t *testing.T) {
	f := NewModule().NewFuncVoid("c")
	a := f.Param("a", ClsD)
	assert.Equal(t, ClsD, f.ClassOf(a))
	assert.Equal(t, ClsW, f.ClassOf(f.Word(1)))
	assert.Equal(t, ClsW, f.ClassOf(R)) // default for non-value refs

	assert.NotNil(t, f.Temp(a))
	assert.Equal(t, "a", f.Temp(a).Name)
	assert.Nil(t, f.Temp(f.Word(1)))
}

func TestBlockSuccs(t *testing.T) {
	f := NewModule().NewFuncVoid("c")
	a := f.Entry()
	b := f.NewBlock("b")
	c := f.NewBlock("c")

	a.Goto(b)
	assert.Equal(t, []*Block{b}, a.Succs())

	b.Jnz(f.Word(1), a, c)
	assert.Equal(t, []*Block{a, c}, b.Succs())

	c.RetVoid()
	assert.Nil(t, c.Succs())

	var hlt Block
	hlt.Jmp = Jmp{Kind: JmpHlt}
	assert.Nil(t, hlt.Succs())
}

func TestModulePrintWithTypeAndData(t *testing.T) {
	m := NewModule()
	m.Types = append(m.Types, &AggType{Name: "pair", Fields: []Field{{Sub: SubW}, {Sub: SubW}}})
	m.Data = append(m.Data, &Data{
		Name:  "nums",
		Align: 4,
		Items: []DataItem{{Sub: SubW, Ints: []int64{1, 2, 3}}},
	})
	f := m.NewFunc("id", ClsW).Export()
	x := f.Param("x", ClsW)
	f.Entry().Ret(x)

	s := m.String()
	assert.Contains(t, s, "type :pair = { w, w }\n")
	assert.Contains(t, s, "data $nums = align 4 { w 1 2 3 }\n")
	assert.Contains(t, s, "export function w $id(w %x) {\n")
}

func TestOpaqueTypeAndDataVariants(t *testing.T) {
	var sb strings.Builder
	printType(&sb, &AggType{Name: "o", Opaque: true, Size: 16, Align: 8})
	assert.Equal(t, "type :o = align 8 { 16 }\n", sb.String())

	sb.Reset()
	printData(&sb, &Data{Name: "s", Items: []DataItem{
		{Str: "hi"},
		{Zero: 4},
		{Sub: SubL, Sym: "g", Off: 8},
	}})
	assert.Equal(t, "data $s = { b \"hi\", z 4, l $g+8 }\n", sb.String())
}

func TestAddTypeAndTypedCall(t *testing.T) {
	m := NewModule()
	tr := m.AddType(&AggType{Name: "pair", Fields: []Field{{Sub: SubW}, {Sub: SubW}}})
	assert.Equal(t, RefType, tr.Kind)
	assert.Equal(t, uint32(0), tr.ID)
	// A second type gets the next id.
	tr2 := m.AddType(&AggType{Name: "trip"})
	assert.Equal(t, uint32(1), tr2.ID)
}

func TestOpInfo(t *testing.T) {
	info := OAdd.Info()
	assert.Equal(t, "add", info.name)
	assert.True(t, info.hasResult)
	assert.True(t, info.commutative)
}

func TestLoadSubAllWidths(t *testing.T) {
	f := NewModule().NewFuncVoid("m")
	p := f.Param("p", ClsL)
	e := f.Entry()
	assert.Equal(t, OLoadl, opOf(e, func() { e.LoadSub(ClsL, SubL, p) }))
	assert.Equal(t, OLoads, opOf(e, func() { e.LoadSub(ClsS, SubS, p) }))
	assert.Equal(t, OLoadd, opOf(e, func() { e.LoadSub(ClsD, SubD, p) }))
}

func TestStoreSubAllWidths(t *testing.T) {
	f := NewModule().NewFuncVoid("m")
	p := f.Param("p", ClsL)
	e := f.Entry()
	assert.Equal(t, OStoreb, opOf(e, func() { e.StoreSub(SubUB, f.Word(1), p) }))
	assert.Equal(t, OStoreh, opOf(e, func() { e.StoreSub(SubUH, f.Word(1), p) }))
	assert.Equal(t, OStorew, opOf(e, func() { e.StoreSub(SubW, f.Word(1), p) }))
	assert.Equal(t, OStorel, opOf(e, func() { e.StoreSub(SubL, f.Long(1), p) }))
	assert.Equal(t, OStores, opOf(e, func() { e.StoreSub(SubS, f.Single(1), p) }))
	assert.Equal(t, OStored, opOf(e, func() { e.StoreSub(SubD, f.Double(1), p) }))
}

func TestVaBuilders(t *testing.T) {
	f := NewModule().NewFuncVoid("v")
	f.Variadic = true
	e := f.Entry()
	ap := e.Alloc(8, 32)
	e.VaStart(ap)
	r := e.VaArg(ClsL, ap)
	assert.Equal(t, OVaStart, e.Instrs[1].Op)
	va := e.Instrs[2]
	assert.Equal(t, OVaArg, va.Op)
	assert.Equal(t, r, va.To)
	assert.Equal(t, ClsL, f.ClassOf(r))
}

func TestUnionLayoutAndPrint(t *testing.T) {
	u := &AggType{Name: "u", Union: true, Cases: [][]Field{
		{{Sub: SubB}},
		{{Sub: SubS}, {Sub: SubW}},
	}}
	sz, al := u.Layout()
	assert.Equal(t, 8, sz) // second case {s,w} is 8 bytes
	assert.Equal(t, 4, al)

	m := NewModule()
	m.AddType(u)
	assert.Contains(t, m.String(), "type :u = { { b } { s, w } }")
}

func TestVariadicPrint(t *testing.T) {
	m := NewModule()
	f := m.NewFunc("g", ClsW)
	f.Variadic = true
	f.Param("x", ClsL)
	f.Entry().Ret(f.Word(0))
	assert.Contains(t, f.String(), "$g(l %x, ...)")

	// A variadic function with no named parameters.
	h := m.NewFuncVoid("h")
	h.Variadic = true
	h.Entry().RetVoid()
	assert.Contains(t, h.String(), "$h(...)")
}

func TestAggTypeLayout(t *testing.T) {
	// { s, b, s }: s@0, b@4, s@8 -> size 12, align 4
	s, a := (&AggType{Fields: []Field{{Sub: SubS}, {Sub: SubB}, {Sub: SubS}}}).Layout()
	assert.Equal(t, 12, s)
	assert.Equal(t, 4, a)

	// { l, b, w }: l@0, b@8, w@12 -> size 16, align 8
	s, a = (&AggType{Fields: []Field{{Sub: SubL}, {Sub: SubB}, {Sub: SubW}}}).Layout()
	assert.Equal(t, 16, s)
	assert.Equal(t, 8, a)

	// inline array: b x 17 -> size 17, align 1
	s, a = (&AggType{Fields: []Field{{Sub: SubB, Count: 17}}}).Layout()
	assert.Equal(t, 17, s)
	assert.Equal(t, 1, a)

	// nested aggregate field takes the nested type's size and alignment
	inner := &AggType{Name: "in", Fields: []Field{{Sub: SubL}}}
	s, a = (&AggType{Fields: []Field{{Sub: SubW}, {Type: inner}}}).Layout()
	assert.Equal(t, 16, s) // w@0, in(align 8)@8 -> 16
	assert.Equal(t, 8, a)

	// opaque reports its declared size and alignment
	s, a = (&AggType{Opaque: true, Size: 24, Align: 16}).Layout()
	assert.Equal(t, 24, s)
	assert.Equal(t, 16, a)

	// explicit alignment raises the minimum
	s, a = (&AggType{Align: 16, Fields: []Field{{Sub: SubW}}}).Layout()
	assert.Equal(t, 16, s)
	assert.Equal(t, 16, a)

	// packed { b, w }: no padding, align 1 -> b@0, w@1, size 5
	s, a = (&AggType{Packed: true, Align: 1, Fields: []Field{{Sub: SubB}, {Sub: SubW}}}).Layout()
	assert.Equal(t, 5, s)
	assert.Equal(t, 1, a)

	// packed leaves are at the packed offsets, so a member can straddle an eightbyte
	leaves, _ := (&AggType{Packed: true, Align: 1, Fields: []Field{{Sub: SubB}, {Sub: SubL}}}).Leaves()
	assert.Equal(t, []Leaf{{Sub: SubB, Off: 0}, {Sub: SubL, Off: 1}}, leaves)

	// packed with an explicit aligned(4) keeps padding out but rounds the size up
	s, a = (&AggType{Packed: true, Align: 4, Fields: []Field{{Sub: SubW}, {Sub: SubB}}}).Layout()
	assert.Equal(t, 8, s) // w@0, b@4, raw size 5 rounded up to align 4
	assert.Equal(t, 4, a)
}

func TestAggTypeWithNestedAndArray(t *testing.T) {
	inner := &AggType{Name: "inner", Fields: []Field{{Sub: SubL}}}
	var sb strings.Builder
	printType(&sb, &AggType{Name: "outer", Fields: []Field{
		{Sub: SubW, Count: 4},
		{Type: inner},
	}})
	assert.Equal(t, "type :outer = { w 4, :inner }\n", sb.String())
}
