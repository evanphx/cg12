package ir

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// richModule builds a module exercising most features: an aggregate type and a
// union, data, source positions, phis, calls, a tail call, and a variadic.
func richModule() *Module {
	pair := &AggType{Name: "pair", Fields: []Field{{Sub: SubW}, {Sub: SubW}}}
	slice := &AggType{Name: "slice", Fields: []Field{{Sub: SubL, Pointer: true}, {Sub: SubL}, {Sub: SubL}}}
	// A union and a nested-aggregate field, to exercise Cases and Field.Type.
	un := &AggType{Name: "u", Union: true, Cases: [][]Field{{{Sub: SubD}}, {{Type: pair}}}}
	m := NewModule()
	m.Types = append(m.Types, pair, slice, un)
	m.Assembly = append(m.Assembly, AssemblyFile{
		PackagePath:  "runtime",
		Path:         "runtime/atomic_arm64.s",
		Source:       "TEXT ·publicationBarrier(SB),$0-0\n",
		Defines:      map[string]int64{"const_offset": 128},
		Includes:     map[string]string{"textflag.h": "#define NOSPLIT 4\n"},
		FloatInputs:  map[string][]int{"floatBits": {0, 8}},
		FloatOutputs: map[string][]int{"floatBits": {16}},
		Signatures: map[string]AsmSignature{
			"runtime_floatBits": {
				Params:  []AsmSlot{{Name: "value", Offset: 0, Cls: ClsD, Width: 8}},
				Results: []AsmSlot{{Name: "bits", Offset: 8, Cls: ClsL, Width: 8}},
			},
		},
	})
	// An opaque frontend attachment under a non-reserved key, to exercise the
	// generic attachment path alongside the reserved assembly one.
	m.Attachments = map[string][]byte{"goc.custom": {0, 1, 2, 'x', 255}}
	m.Data = append(m.Data, &Data{
		Name: "tbl", Linkage: Linkage{Export: true}, Align: 8,
		Items:        []DataItem{{Sub: SubW, Ints: []int64{1, 2, 3}}, {Sym: "tbl", Off: 4}, {Str: "hi"}, {Zero: 4}},
		PointerWords: []int{2, 5},
	})
	fi := m.File("prog.src")

	// sumpair(:pair %p) with a branch and a phi-carried return.
	sp := m.NewFunc("sumpair", ClsW).Export()
	p := sp.Param("p", ClsP)
	sp.Temp(p).Agg = pair
	e := sp.Entry()
	ra, rb, end := sp.NewBlock("ra"), sp.NewBlock("rb"), sp.NewBlock("end")
	ra.SecondaryEntry = true
	rb.SyntheticSuccs = []*Block{ra}
	e.At(SrcPos{File: fi, Line: 3, Col: 1})
	a := e.Load(ClsW, p)
	b := e.Load(ClsW, e.Add(ClsP, p, sp.ConstInt(ClsP, 4)))
	e.Jnz(e.Cmp(CmpSgt, ClsW, a, b), ra, rb)
	ra.Goto(end)
	rb.Goto(end)
	r := end.Phi(ClsW, PhiEdge{From: ra, Val: a}, PhiEdge{From: rb, Val: b})
	end.Ret(r)

	// go(n): tail-call sumpair; also a variadic function to exercise that flag.
	g := m.NewFunc("go", ClsW).Export()
	n := g.Param("n", ClsW)
	g.Entry().TailCall(ClsW, g.Sym("sumpair", 0), n)
	g.Entry().Instrs[0].CallConv = CallConvGoInternal
	g.Entry().Instrs[0].CallConvSet = true

	v := m.NewFunc("vf", ClsD)
	v.Variadic = true
	v.CallConv = CallConvGoInternal
	v.ManagedFrame = true
	v.NoSplit = true
	v.SystemStack = true
	v.HasClosureContext = true
	// A function that receives a closure context has the temporary that receives
	// it: the flag and the mark are two halves of one fact, and ir.Verify -- which
	// DecodeModule runs -- rejects a function that states only one of them.
	v.Temp(v.NewTemp("closure", ClsP)).ClosureContext = true
	v.Param("k", ClsW)
	v.Entry().Ret(v.Double(1.5))

	// tail([]byte) []byte exercises grouped by-value parameters and results.
	tail := m.NewFunc("tail", ClsP)
	tail.RetAgg = slice
	tail.RetValues = true
	sliceParts := tail.ParamGroup("value", slice, ClsP, ClsL, ClsL)
	tail.Aggregate(slice, sliceParts...)
	tail.Entry().RetAggregate(sliceParts...)

	caller := m.NewFunc("calltail", ClsP)
	callParts := caller.Entry().CallAggregate(slice, []Cls{ClsP, ClsL, ClsL}, caller.Sym("tail", 0), caller.ConstInt(ClsP, 0), caller.Long(2), caller.Long(2))
	call := &caller.Entry().Instrs[0]
	call.ArgGroups = []ValueGroup{{Index: 0, Count: 3, Type: slice}}
	caller.RetAgg = slice
	caller.RetValues = true
	caller.Entry().RetAggregate(callParts...)

	// sw(k): a multiway switch, to exercise the JmpSwitch terminator round-trip.
	sw := m.NewFunc("sw", ClsW).Export()
	k := sw.Param("k", ClsW)
	c1, c2, dflt := sw.NewBlock("c1"), sw.NewBlock("c2"), sw.NewBlock("dflt")
	sw.Entry().Switch(k, dflt, true, []SwitchCase{{Val: -1, Blk: c1}, {Val: 100, Blk: c2}})
	c1.Ret(sw.ConstInt(ClsW, 10))
	c2.Ret(sw.ConstInt(ClsW, 20))
	dflt.Ret(sw.ConstInt(ClsW, 0))
	return m
}

func TestSwitchSuccs(t *testing.T) {
	f := NewModule().NewFunc("f", ClsW)
	k := f.Param("k", ClsW)
	a, b, d := f.NewBlock("a"), f.NewBlock("b"), f.NewBlock("d")
	f.Entry().Switch(k, d, false, []SwitchCase{{Val: 1, Blk: a}, {Val: 2, Blk: b}})
	// Successors are the default followed by every case block.
	assert.Equal(t, []*Block{d, a, b}, f.Entry().Succs())
}

func TestBinaryRoundTrip(t *testing.T) {
	m := richModule()
	data, err := m.MarshalBinary()
	require.NoError(t, err)
	assert.Greater(t, len(data), 0)

	m2, err := DecodeModule(data)
	require.NoError(t, err)

	// Textual printing is a strong structural comparison of the whole module.
	assert.Equal(t, m.String(), m2.String())
	assert.Equal(t, m.Files, m2.Files)
	assert.Equal(t, m.Assembly, m2.Assembly)
	assert.Equal(t, m.Attachments, m2.Attachments)
	assert.Equal(t, m.Data[0].PointerWords, m2.Data[0].PointerWords)
	assert.True(t, m2.Funcs[0].Blocks[1].SecondaryEntry)
	require.Len(t, m2.Funcs[0].Blocks[2].SyntheticSuccs, 1)
	assert.Same(t, m2.Funcs[0].Blocks[1], m2.Funcs[0].Blocks[2].SyntheticSuccs[0])
	// The decoded aggregate reference is resolved, not nil.
	require.NotNil(t, m2.Funcs[0].Params[0].Agg)
	assert.Equal(t, "pair", m2.Funcs[0].Params[0].Agg.Name)

	var tail *Func
	var caller *Func
	for _, function := range m2.Funcs {
		switch function.Name {
		case "tail":
			tail = function
		case "calltail":
			caller = function
		}
	}
	require.NotNil(t, tail)
	assert.True(t, tail.RetValues)
	require.Len(t, tail.ParamGroups, 1)
	assert.Equal(t, ValueGroup{Index: 0, Count: 3, Type: m2.Types[1]}, tail.ParamGroups[0])
	require.Len(t, tail.AggregateValues, 1)
	assert.Len(t, tail.AggregateValues[0].Parts, 3)

	require.NotNil(t, caller)
	require.Len(t, caller.Entry().Instrs, 1)
	call := &caller.Entry().Instrs[0]
	assert.True(t, call.RetValues)
	require.Len(t, call.ArgGroups, 1)
	assert.Equal(t, ValueGroup{Index: 0, Count: 3, Type: m2.Types[1]}, call.ArgGroups[0])
}

func TestBinaryDeterministic(t *testing.T) {
	m := richModule()
	a, _ := m.MarshalBinary()
	b, _ := m.MarshalBinary()
	assert.Equal(t, a, b, "encoding is stable, so units are content-addressable")
}

func TestBinaryEmptyModule(t *testing.T) {
	data, err := NewModule().MarshalBinary()
	require.NoError(t, err)
	m, err := DecodeModule(data)
	require.NoError(t, err)
	assert.Empty(t, m.Funcs)
}

func TestBinaryRejectsBadMagic(t *testing.T) {
	_, err := DecodeModule([]byte("not a unit at all"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "magic")

	_, err = DecodeModule(nil)
	require.Error(t, err)
}

func TestBinaryRejectsWrongVersion(t *testing.T) {
	data, _ := NewModule().MarshalBinary()
	data[len(binMagic)] = binVersion + 1 // corrupt the version byte
	_, err := DecodeModule(data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version")
}

func TestBinaryRejectsTruncated(t *testing.T) {
	data, _ := richModule().MarshalBinary()
	_, err := DecodeModule(data[:len(data)-5]) // cut off the tail
	require.Error(t, err)
}

// The binary encoding writes a Ref's kind and an Op as their numbers, so those
// numbers are the format. Two RefKinds and one Op were removed for being unused;
// their slots are burned (`_`) rather than reclaimed, because closing the gap
// would renumber everything after and make an already-encoded module decode as
// different instructions -- silently, since the decoder would find kinds and ops
// it knows perfectly well.
//
// This pins the numbers a future cleanup would quietly change. If it fails,
// either restore the burned slots or bump binVersion and mean it.
func TestWireNumbersSurviveTheBurnedSlots(t *testing.T) {
	assert.Equal(t, RefKind(7), RefReg, "RefReg sits after the two burned RefKinds")
	assert.Equal(t, Op(58), OPar, "OPar sits after OHeapAlloc and the burned OArgEnv")

	// The burned op's slot must not have acquired a live op: the array is indexed
	// by Op, so a new op landing there would answer to the dead one's number.
	assert.Empty(t, opTable[OArg+1].name, "the slot after OArg is the burned OArgEnv")
	assert.Empty(t, opTable[OSetReg+1].name, "the slot after OSetReg is the burned OGetCallerPC")
	assert.Empty(t, opTable[OSetReg+2].name, "the second slot after OSetReg is the burned OGetCallerSP")

}

// The format carries a content digest of its own payload, so a unit that is the
// right version and not the right bytes is a decode error rather than a module.
//
// Before it, the only integrity check a reader had was structural -- and the
// memo cannot use the structural one (see ir.DecodeModuleUnverified), so it
// bolted a sha256 on from outside. A digest a caller has to remember to keep is
// a digest some caller does not keep; in the header it is the decoder's job.
func TestBinaryRejectsCorruptedContent(t *testing.T) {
	data, err := richModule().MarshalBinary()
	require.NoError(t, err)

	// Every byte of the payload is covered: flip one in the middle of the funcs.
	corrupt := append([]byte(nil), data...)
	corrupt[len(corrupt)-16] ^= 0x01
	_, err = DecodeModule(corrupt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "digest")

	// A payload byte immediately after the header, too -- the digest starts where
	// the header ends, so an off-by-one there would leave the first field unguarded.
	corrupt = append([]byte(nil), data...)
	corrupt[binHeaderSize] ^= 0x01
	_, err = DecodeModule(corrupt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "digest")

	// And the check is not vacuous: the untouched bytes decode.
	_, err = DecodeModule(data)
	require.NoError(t, err)
}

// The digest is checked before the payload is read, so it also catches the
// corruption a structural decode would have accepted -- and it is checked by
// DecodeModuleUnverified too, which is the entry point a cache uses precisely
// because it cannot run the verifier.
func TestBinaryDigestGuardsTheUnverifiedPath(t *testing.T) {
	data, err := richModule().MarshalBinary()
	require.NoError(t, err)
	corrupt := append([]byte(nil), data...)
	corrupt[len(corrupt)-16] ^= 0x01
	_, err = DecodeModuleUnverified(corrupt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "digest")
}

// A version mismatch reports itself as one. A stale cache is the ordinary case
// and the diagnostic that names it is worth more than "corrupt".
func TestBinaryReportsVersionBeforeDigest(t *testing.T) {
	data, err := richModule().MarshalBinary()
	require.NoError(t, err)
	data[len(binMagic)] = binVersion + 1
	_, err = DecodeModule(data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version")
	assert.NotContains(t, err.Error(), "digest")
}

// The digest is a function of the payload, so it does not disturb the property
// the cache is built on: equal modules encode to equal bytes.
func TestBinaryDigestIsDeterministic(t *testing.T) {
	m := richModule()
	a, err := m.MarshalBinary()
	require.NoError(t, err)
	b, err := m.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, a, b)
	assert.Equal(t, a[len(binMagic)+1:binHeaderSize], b[len(binMagic)+1:binHeaderSize])
	assert.NotEqual(t, make([]byte, binDigestSize), a[len(binMagic)+1:binHeaderSize],
		"the reserved digest bytes must have been filled in")
}
