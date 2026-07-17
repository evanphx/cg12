package ir

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClsPredicates(t *testing.T) {
	assert.True(t, ClsW.IsInt())
	assert.True(t, ClsL.IsInt())
	assert.False(t, ClsS.IsInt())
	assert.False(t, ClsD.IsInt())

	assert.False(t, ClsW.IsFloat())
	assert.True(t, ClsS.IsFloat())
	assert.True(t, ClsD.IsFloat())
}

func TestClsSizeAndString(t *testing.T) {
	cases := []struct {
		c    Cls
		size int
		str  string
	}{
		{ClsW, 4, "w"},
		{ClsL, 8, "l"},
		{ClsS, 4, "s"},
		{ClsD, 8, "d"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.size, tc.c.Size(), tc.str)
		assert.Equal(t, tc.str, tc.c.String())
	}
	assert.Equal(t, 0, Cls(99).Size())
	assert.Equal(t, "?", Cls(99).String())
}

func TestSubCls(t *testing.T) {
	cases := []struct {
		s    SubCls
		cls  Cls
		size int
		str  string
	}{
		{SubB, ClsW, 1, "sb"},
		{SubUB, ClsW, 1, "ub"},
		{SubH, ClsW, 2, "sh"},
		{SubUH, ClsW, 2, "uh"},
		{SubW, ClsW, 4, "w"},
		{SubL, ClsL, 8, "l"},
		{SubS, ClsS, 4, "s"},
		{SubD, ClsD, 8, "d"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.cls, tc.s.Cls(), tc.str)
		assert.Equal(t, tc.size, tc.s.Size(), tc.str)
		assert.Equal(t, tc.str, tc.s.String())
	}
	assert.Equal(t, 0, SubCls(99).Size())
	assert.Equal(t, "?", SubCls(99).String())
}

func TestSubClsElemString(t *testing.T) {
	cases := map[SubCls]string{
		SubB: "b", SubUB: "b", SubH: "h", SubUH: "h",
		SubW: "w", SubL: "l", SubS: "s", SubD: "d",
	}
	for s, want := range cases {
		assert.Equalf(t, want, s.ElemString(), "%v", s)
	}
	assert.Equal(t, "w", SubCls(99).ElemString()) // default
}

func TestOpMetadata(t *testing.T) {
	assert.True(t, OAdd.HasResult())
	assert.True(t, OAdd.IsCommutative())
	assert.True(t, OMul.IsCommutative())
	assert.False(t, OSub.IsCommutative())
	assert.True(t, OAnd.IsCommutative())

	assert.False(t, OStorew.HasResult())
	assert.True(t, OLoadl.HasResult())

	assert.Equal(t, "add", OAdd.String())
	assert.Equal(t, "sub", OSub.String())
	assert.Equal(t, "storew", OStorew.String())
	assert.Equal(t, "", OCmp.String()) // computed at print time
}

func TestOpFromString(t *testing.T) {
	o, ok := OpFromString("add")
	assert.True(t, ok)
	assert.Equal(t, OAdd, o)

	o, ok = OpFromString("loadsw")
	assert.True(t, ok)
	assert.Equal(t, OLoadsw, o)

	_, ok = OpFromString("csltw") // compares are not named ops
	assert.False(t, ok)
	_, ok = OpFromString("nonsense")
	assert.False(t, ok)
}

func TestOpClassification(t *testing.T) {
	loads := []Op{OLoadsb, OLoadub, OLoadsh, OLoaduh, OLoadsw, OLoaduw, OLoadl, OLoads, OLoadd}
	for _, o := range loads {
		assert.True(t, o.IsLoad(), o.String())
		assert.False(t, o.IsStore(), o.String())
		assert.False(t, o.IsAlloc(), o.String())
	}
	stores := []Op{OStoreb, OStoreh, OStorew, OStorel, OStores, OStored}
	for _, o := range stores {
		assert.True(t, o.IsStore(), o.String())
		assert.False(t, o.IsLoad(), o.String())
	}
	allocs := []Op{OAlloc4, OAlloc8, OAlloc16}
	for _, o := range allocs {
		assert.True(t, o.IsAlloc(), o.String())
		assert.False(t, o.IsLoad(), o.String())
	}
	assert.False(t, OAdd.IsLoad())
	assert.False(t, OAdd.IsStore())
	assert.False(t, OAdd.IsAlloc())
}

// Every opcode below numOps must have a table entry (non-nil name) unless it is
// intentionally blank: OCmp, whose mnemonic is computed from its predicate, and
// the slot after OArg, which was OArgEnv and is burned to keep the wire numbers
// of every op after it (see TestWireNumbersSurviveTheBurnedSlots).
func TestOpTableComplete(t *testing.T) {
	for o := Op(0); o < numOps; o++ {
		if o == OCmp || o == OArg+1 {
			continue
		}
		assert.NotEmptyf(t, opTable[o].name, "op %d missing name", o)
	}
}

func TestCmpPredicates(t *testing.T) {
	for c := Cmp(0); c < CmpFeq; c++ {
		assert.Falsef(t, c.IsFloat(), "int pred %d", c)
	}
	for c := CmpFeq; c < numCmps; c++ {
		assert.Truef(t, c.IsFloat(), "float pred %d", c)
	}
	assert.Equal(t, "slt", CmpSlt.String())
	assert.Equal(t, "eq", CmpEq.String())
	assert.Equal(t, "uo", CmpFuo.String())
	assert.Equal(t, "o", CmpFo.String())
}

func TestRefHelpers(t *testing.T) {
	assert.True(t, R.IsNone())
	assert.Equal(t, RefNone, R.Kind)

	tr := tempRef(3)
	assert.True(t, tr.IsTemp())
	assert.False(t, tr.IsConst())
	assert.False(t, tr.IsNone())
	assert.Equal(t, uint32(3), tr.ID)

	cr := constRef(5)
	assert.True(t, cr.IsConst())
	assert.False(t, cr.IsTemp())

	assert.Equal(t, RefType, typeRef(1).Kind)
	assert.Equal(t, RefSlot, slotRef(2).Kind)
}

func TestInstrArg(t *testing.T) {
	in := Instr{Args: []Ref{tempRef(1), tempRef(2)}}
	assert.Equal(t, tempRef(1), in.Arg(0))
	assert.Equal(t, tempRef(2), in.Arg(1))
	assert.True(t, in.Arg(2).IsNone())
	assert.True(t, in.Arg(-1).IsNone())
}

// A backend-private op is a lowering artifact, not portable IL, so the text
// parser must refuse it even though it has a printed name.
//
// arm64 rewrites idioms and thread-local access into ops it alone produces and
// consumes -- rotr, bic, the TLS trio. They still print (a lowered function has
// to be readable when debugging), but a .ssa file naming one is not a portable
// module: handed to another backend it is at best an "unsupported op", and the
// text format is supposed to be the target-independent one.
func TestPrivateOpsDoNotParse(t *testing.T) {
	for _, o := range []Op{ORotr, OBic, OThreadPtr, OTLSOffset, OTLSIndexAddr} {
		name := o.String()
		assert.NotEmptyf(t, name, "op %d must still print for debugging", int(o))
		_, ok := OpFromString(name)
		assert.Falsef(t, ok, "%q is backend-private and must not parse as portable IL", name)
	}
	// The portable ops these sit among still parse -- including clz, which reads
	// like an arm64 thing but is generic (__builtin_clz, any target).
	for _, name := range []string{"add", "and", "clz", "loadl"} {
		_, ok := OpFromString(name)
		assert.Truef(t, ok, "%q is portable and must parse", name)
	}
}
