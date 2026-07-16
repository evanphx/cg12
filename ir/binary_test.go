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
	// A union and a nested-aggregate field, to exercise Cases and Field.Type.
	un := &AggType{Name: "u", Union: true, Cases: [][]Field{{{Sub: SubD}}, {{Type: pair}}}}
	m := NewModule()
	m.Types = append(m.Types, pair, un)
	m.Assembly = append(m.Assembly, AssemblyFile{
		PackagePath: "runtime",
		Path:        "runtime/atomic_arm64.s",
		Source:      "TEXT ·publicationBarrier(SB),$0-0\n",
		Defines:     map[string]int64{"const_offset": 128},
		Includes:    map[string]string{"textflag.h": "#define NOSPLIT 4\n"},
	})
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

	v := m.NewFunc("vf", ClsD)
	v.Variadic = true
	v.Param("k", ClsW)
	v.Entry().Ret(v.Double(1.5))

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
	assert.Equal(t, m.Data[0].PointerWords, m2.Data[0].PointerWords)
	// The decoded aggregate reference is resolved, not nil.
	require.NotNil(t, m2.Funcs[0].Params[0].Agg)
	assert.Equal(t, "pair", m2.Funcs[0].Params[0].Agg.Name)
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
	assert.Equal(t, Op(57), OPar, "OPar sits after the burned OArgEnv")

	// The burned op's slot must not have acquired a live op: the array is indexed
	// by Op, so a new op landing there would answer to the dead one's number.
	assert.Empty(t, opTable[OArg+1].name, "the slot after OArg is the burned OArgEnv")

}
