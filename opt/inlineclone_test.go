package opt

import (
	"reflect"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clonedInstrFields lists the ir.Instr fields cloneInstr must carry into the
// caller, and TestCloneInstrCopiesEveryClonedField checks that it does.
//
// [ir.CloneFunc] copies a function by round-tripping it through the binary
// encoder specifically to avoid a hand-written walk, whose failure mode it
// names: "correct until someone adds a field, and then it is silently wrong in
// whatever depends on that field". cloneInstr is that hand-written walk, and it
// has now been silently wrong twice -- once dropping Volatile, once dropping
// CallConv/CallConvSet, which re-lowered an inlined ABIInternal closure call as
// AAPCS64 and returned a three-word aggregate from a buffer nobody wrote. It
// cannot become an encoder round trip, because half the point of the walk is
// remapping refs and blocks into the caller's numbering, so the classification
// is written down here instead.
//
// Adding a field to ir.Instr means deciding which list it belongs on.
var clonedInstrFields = []string{
	"Op",
	"Cls",
	"To",
	"Cmp",
	"Aux",
	"Amode",
	"RetAgg",
	"RetValues",
	"Tail",
	"Volatile",
	"ClosureCall",
	"ClosureContext",
	"CallConv",
	"CallConvSet",
	"Asm",
	"Intrin",
	"Pos",
	"Args",
	"Blk",
}

// uncopiedInstrFields lists the fields cloneInstr deliberately does not assign
// from the original, each because something else owns them.
var uncopiedInstrFields = []string{
	// Extra is not copied as a pointer -- that would leave the clone sharing the
	// original's side table, so a later SetStackResult on one would move the
	// other's. Its contents are copied through the accessors instead, which is
	// what TestCloneInstrCopiesEveryClonedField checks for the Extra-backed
	// fields (Defs, StackResult, Unroll, AggArgs, ArgGroups).
	"Extra",
	// Inl is the inline provenance chain, and it is the one thing the clone must
	// NOT inherit unchanged: spliceCall rebases it onto the call site it is
	// splicing, so a clone that kept the callee's chain would attribute the
	// inlined code to the wrong caller.
	"Inl",
}

func TestCloneInstrClassifiesEveryInstrField(t *testing.T) {
	classified := make(map[string]bool, len(clonedInstrFields)+len(uncopiedInstrFields))
	for _, name := range clonedInstrFields {
		classified[name] = true
	}
	for _, name := range uncopiedInstrFields {
		classified[name] = true
	}

	instrType := reflect.TypeOf(ir.Instr{})
	for index := 0; index < instrType.NumField(); index++ {
		name := instrType.Field(index).Name
		assert.True(t, classified[name],
			"ir.Instr field %s is on neither clonedInstrFields nor uncopiedInstrFields: "+
				"decide whether cloneInstr must carry it into the caller and say so there", name)
	}
	for name := range classified {
		_, ok := instrType.FieldByName(name)
		assert.True(t, ok, "%s is classified but ir.Instr has no such field", name)
	}
}

func TestCloneInstrCopiesEveryClonedField(t *testing.T) {
	// An instruction with every field set to something distinguishable from its
	// zero value, so a field the walk forgets shows up as a zero in the clone.
	module := ir.NewModule()
	function := module.NewFunc("f", ir.ClsL)
	entry := function.Entry()
	entry.RetVoid()

	temp := function.NewTemp("t", ir.ClsL)
	context := function.NewTemp("ctx", ir.ClsP)
	defined := function.NewTemp("def", ir.ClsL)
	slot := function.NewTemp("slot", ir.ClsL)
	aggregate := &ir.AggType{Name: "agg"}

	original := ir.Instr{
		Op:             ir.OCall,
		Cls:            ir.ClsL,
		To:             temp,
		Cmp:            ir.CmpNe,
		Aux:            9,
		Amode:          3,
		RetAgg:         aggregate,
		RetValues:      true,
		Tail:           true,
		Volatile:       true,
		ClosureCall:    true,
		ClosureContext: context,
		CallConv:       ir.CallConvGoInternal,
		CallConvSet:    true,
		Asm:            &ir.AsmOp{Template: "nop"},
		Intrin:         &ir.IntrinOp{Name: "intrinsic"},
		Pos:            ir.SrcPos{File: 1, Line: 2, Col: 3},
		Args:           []ir.Ref{function.Long(0), temp},
		Blk:            entry,
	}
	original.AddDef(defined)
	original.SetStackResult(slot, 16)
	original.SetUnroll(4)
	original.SetAggArgs([]*ir.AggType{aggregate})
	original.SetArgGroups([]ir.ValueGroup{{Count: 1}})

	identityRef := func(r ir.Ref) ir.Ref { return r }
	identityBlock := func(b *ir.Block) *ir.Block { return b }
	clone := cloneInstr(function, &original, identityRef, identityBlock)

	// With identity mapping, every field cloneInstr carries must come out equal
	// to the field it came from.
	originalValue := reflect.ValueOf(original)
	cloneValue := reflect.ValueOf(clone)
	for _, name := range clonedInstrFields {
		field, ok := reflect.TypeOf(ir.Instr{}).FieldByName(name)
		require.True(t, ok, "ir.Instr has a field named %s", name)
		assert.Equal(t,
			originalValue.FieldByIndex(field.Index).Interface(),
			cloneValue.FieldByIndex(field.Index).Interface(),
			"cloneInstr carried %s into the clone", name)
	}

	// The Extra-backed fields travel through the accessors rather than by
	// copying the pointer, so check both that they arrived and that the clone
	// got its own side table.
	assert.Equal(t, []ir.Ref{defined}, clone.Defs(), "Defs")
	assert.Equal(t, slot, clone.StackResult(), "StackResult")
	assert.Equal(t, int64(16), clone.StackResultOffset(), "StackResultOffset")
	assert.Equal(t, int32(4), clone.Unroll(), "Unroll")
	assert.Equal(t, []*ir.AggType{aggregate}, clone.AggArgs(), "AggArgs")
	assert.Equal(t, []ir.ValueGroup{{Count: 1}}, clone.ArgGroups(), "ArgGroups")
	assert.NotSame(t, original.Extra, clone.Extra, "the clone has its own InstrExtra")
}
