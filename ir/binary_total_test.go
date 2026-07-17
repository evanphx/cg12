package ir

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// The binary format exists so a cached module can skip the front end, which only
// works if a decoded module is the module. It is a field-by-field encoder with
// nothing to keep it honest, so every field added since it was written was
// silently dropped: the inline-asm template, the inline context, the GC roots.
// A cached module with inline asm decoded to an OAsm with no template and
// panicked the backend on a nil dereference; a cached module's GC roots decoded
// to none at all, so its stack maps came back empty.
//
// These tests set every field of the types the format carries, and require the
// round-trip to return them. The zero-value check is what makes that durable: a
// new field is zero in the fixture, so the test fails and names it, and whoever
// added it has to set it here -- at which point the round-trip check forces the
// encoder to carry it.

// allFieldsSet requires every field of a struct to be non-zero, so that a
// round-trip comparison actually exercises each one.
func allFieldsSet(t *testing.T, v any, skip ...string) {
	t.Helper()
	skipped := map[string]bool{}
	for _, s := range skip {
		skipped[s] = true
	}
	rv := reflect.ValueOf(v)
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if skipped[f.Name] || !f.IsExported() {
			continue
		}
		require.Falsef(t, rv.Field(i).IsZero(),
			"%s.%s is zero in this fixture, so the round-trip never checks it. "+
				"Set it here (and teach ir/binary.go to carry it) -- a field the "+
				"encoder drops is a cached module that silently is not the module.",
			rt.Name(), f.Name)
	}
}

func TestInstrRoundTripsEveryField(t *testing.T) {
	agg := &AggType{Name: "pair", Fields: []Field{{Sub: SubW}, {Sub: SubW}}}
	in := Instr{
		Op:                OCall,
		Cls:               ClsL,
		To:                Ref{Kind: RefTemp, ID: 1},
		Args:              []Ref{{Kind: RefTemp, ID: 2}, {Kind: RefTemp, ID: 3}},
		Cmp:               CmpSlt,
		Aux:               42,
		Unroll:            3,
		Amode:             9,
		AggArgs:           []*AggType{agg},
		ArgGroups:         []ValueGroup{{Index: 0, Count: 1, Type: agg}},
		Defs:              []Ref{{Kind: RefTemp, ID: 4}},
		RetAgg:            agg,
		RetValues:         true,
		StackResult:       Ref{Kind: RefTemp, ID: 4},
		StackResultOffset: 8,
		Pos:               SrcPos{File: 1, Line: 2, Col: 3},
		Tail:              true,
		Volatile:          true,
		ClosureCall:       true,
		Asm: &AsmOp{
			Template: "movl %k1, %k0",
			Ops:      []AsmOperandKind{AsmRegOut, AsmRegIn},
			Regs:     []string{"", "a"},
			Clobbers: []string{"cc"},
		},
		Inl: &InlineSite{
			Callee: "inner",
			Call:   SrcPos{File: 1, Line: 9, Col: 1},
			Parent: &InlineSite{Callee: "outer", Call: SrcPos{File: 1, Line: 4, Col: 2}},
		},
		Blk:    &Block{Name: "target"},
		Intrin: &IntrinOp{Name: "stacksave"},
	}
	allFieldsSet(t, in)
	// The carried structs need the same treatment: a field added to one of them
	// is a field the encoder can drop just as quietly, and enumerating only
	// Instr's own fields does not see it.
	allFieldsSet(t, *in.Asm)
	allFieldsSet(t, *in.Inl, "Parent") // the innermost site has no parent to set
	allFieldsSet(t, *in.Intrin)

	got := roundTripInstr(t, in)

	// Blk is compared by name: the decoder rebuilds blocks, so it is a different
	// pointer to the same block.
	require.Equal(t, in.Blk.Name, got.Blk.Name)
	in.Blk, got.Blk = nil, nil
	// AggType likewise round-trips through the module's type table by identity.
	require.Equal(t, in.RetAgg.Name, got.RetAgg.Name)
	require.Len(t, got.AggArgs, 1)
	require.Len(t, got.ArgGroups, 1)
	require.Equal(t, in.ArgGroups[0].Type.Name, got.ArgGroups[0].Type.Name)
	in.ArgGroups[0].Type, got.ArgGroups[0].Type = nil, nil
	in.AggArgs, got.AggArgs, in.RetAgg, got.RetAgg = nil, nil, nil, nil

	require.Equal(t, in, got)
}

func TestTempRoundTripsEveryField(t *testing.T) {
	tmp := Temp{
		ID:     0,
		Name:   "x",
		Cls:    ClsL,
		Slot:   8,
		Reg:    3,
		Fixed:  true,
		Agg:    &AggType{Name: "pair", Fields: []Field{{Sub: SubW}}},
		GCRef:  true,
		GCType: 7,
	}
	allFieldsSet(t, tmp, "ID") // ID is the index, restored by position

	m := NewModule()
	f := m.NewFunc("f", ClsW)
	f.Temps = []*Temp{&tmp}
	f.Entry().RetVoid()

	data, err := m.MarshalBinary()
	require.NoError(t, err)
	back, err := DecodeModule(data)
	require.NoError(t, err)

	got := *back.Funcs[0].Temps[0]
	require.Equal(t, tmp.Agg.Name, got.Agg.Name)
	tmp.Agg, got.Agg = nil, nil
	require.Equal(t, tmp, got)
}

// roundTripInstr puts one instruction through a whole module encode/decode.
func roundTripInstr(t *testing.T, in Instr) Instr {
	t.Helper()
	m := NewModule()
	m.AddType(in.RetAgg)
	f := m.NewFunc("f", ClsW)
	for i := 0; i < 5; i++ {
		f.NewTemp("t", ClsW)
	}
	target := f.NewBlock(in.Blk.Name)
	target.RetVoid()
	e := f.Entry()
	e.Instrs = append(e.Instrs, in)
	// in reads %2 and %3; define them so the module is well-formed SSA (decode
	// verifies). Order within a block is not checked, so these can follow in,
	// keeping it at index 0 for the round-trip comparison.
	e.Instrs = append(e.Instrs,
		Instr{Op: OCopy, Cls: ClsW, To: Ref{Kind: RefTemp, ID: 2}, Args: []Ref{f.Word(0)}},
		Instr{Op: OCopy, Cls: ClsW, To: Ref{Kind: RefTemp, ID: 3}, Args: []Ref{f.Word(0)}},
	)
	e.Goto(target)

	data, err := m.MarshalBinary()
	require.NoError(t, err)
	back, err := DecodeModule(data)
	require.NoError(t, err)
	return back.Funcs[0].Start.Instrs[0]
}
