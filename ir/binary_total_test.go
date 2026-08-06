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
//
// Unexported fields count. They used to be exempt, and two of the format's
// losses were hiding behind that exemption: ir.Func.nameSeq, whose absence made
// a decoded module fail to assemble ("duplicate label"), and ir.Func.lowered,
// whose absence disarmed the verifier's SSA gate, the interpreter's refusal to
// run lowered IR, and MarkLowered's cross-target guard. A field being unexported
// says who may write it, not whether the format has to carry it -- so an
// exception has to be spelled with its name in skip, where the next reader sees
// that somebody decided.
//
// It also records the type, so TestEveryTypeTheFormatCarriesIsGuarded can check
// that the list of guarded types is not a list of types nothing guards.
func allFieldsSet(t *testing.T, v any, skip ...string) {
	t.Helper()
	skipped := map[string]bool{}
	for _, s := range skip {
		skipped[s] = true
	}
	rv := reflect.ValueOf(v)
	rt := rv.Type()
	guardedTypes[rt.Name()] = true
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if skipped[f.Name] {
			continue
		}
		require.Falsef(t, rv.Field(i).IsZero(),
			"%s.%s is zero in this fixture, so the round-trip never checks it. "+
				"Set it here (and teach ir/binary.go to carry it) -- a field the "+
				"encoder drops is a cached module that silently is not the module.",
			rt.Name(), f.Name)
	}
}

// guardedTypes records every struct type some test put through allFieldsSet.
var guardedTypes = map[string]bool{}

// TestDataRoundTripsEveryField covers a data definition and its items.
//
// DataItem.RelativeTo was the drop this found. Every abi.Type goc emits carries
// three of them -- the name offset, the type offset, and each method's two
// offset words are 32-bit displacements from the module's data base, not
// addresses -- and arm64/mc.go takes a different relocation path for a relative
// item than for an absolute one. A round trip demoted 1639 of them on a
// hello-world, and nothing noticed: ir.Verify does not look at Data at all, and
// memo.DataDigest digests through this same encoder, so it could not see a
// difference the encoder had already erased.
func TestDataRoundTripsEveryField(t *testing.T) {
	d := &Data{
		Name:    "tbl",
		Linkage: Linkage{Export: true, Thread: true, Section: ".mydata", SecArgs: "aw"},
		Align:   16,
		Items: []DataItem{{
			Sub:        SubW,
			Zero:       4,
			Ints:       []int64{1, 2},
			Flts:       []float64{1.5},
			Sym:        "other",
			RelativeTo: "base",
			Off:        8,
			Str:        "hi",
		}},
		PointerWords: []int{2, 5},
		GoTypeLink:   true,
	}
	allFieldsSet(t, *d)
	allFieldsSet(t, d.Items[0])
	allFieldsSet(t, d.Linkage)

	m := NewModule()
	m.Data = append(m.Data, d)
	data, err := m.MarshalBinary()
	require.NoError(t, err)
	back, err := DecodeModule(data)
	require.NoError(t, err)
	require.Equal(t, d, back.Data[0])
}

// TestConstRoundTripsEveryField covers a constant.
//
// Const.Thread was the drop this found. A thread-local symbol constant is
// addressed through the TLS ABI; without the flag it decodes as an ordinary
// symbol address, which is a different address in every thread. Note that
// internConst's key does include Thread (ir/build.go), so the two constants are
// distinct before the round trip and merge into one after it.
func TestConstRoundTripsEveryField(t *testing.T) {
	c := Const{Kind: ConstSym, Cls: ClsL, Int: 16, Flt: 1.5, Sym: "g", Thread: true}
	allFieldsSet(t, c)

	m := NewModule()
	f := m.NewFunc("f", ClsW)
	f.Consts = []Const{c}
	f.Entry().RetVoid()

	data, err := m.MarshalBinary()
	require.NoError(t, err)
	back, err := DecodeModule(data)
	require.NoError(t, err)
	require.Equal(t, c, back.Funcs[0].Consts[0])
}

// TestAggTypeRoundTripsEveryField covers an aggregate type and its fields.
//
// AggType.Packed was the drop this found. AggType.walk reads it (ir/type.go:282,
// :291) to place members with no padding, so a packed struct that lost the flag
// answers Layout() with a different size and alignment than it had -- and the
// stack slot, the by-value ABI classification and every field offset move with it.
func TestAggTypeRoundTripsEveryField(t *testing.T) {
	inner := &AggType{Name: "inner", Fields: []Field{{Sub: SubW, Count: 2, Pointer: true}}}
	agg := &AggType{
		Name:   "packed",
		Align:  1,
		Size:   9,
		Opaque: true,
		Packed: true,
		Fields: []Field{{Sub: SubB, Type: inner, Count: 3, Pointer: true}},
		Union:  true,
		Cases:  [][]Field{{{Sub: SubL, Type: inner, Count: 2, Pointer: true}}},
	}
	allFieldsSet(t, *agg, "Fields") // Field.Sub's zero value is SubB, so Fields is checked below
	allFieldsSet(t, agg.Fields[0], "Sub")
	allFieldsSet(t, agg.Cases[0][0])

	m := NewModule()
	m.AddType(agg)
	data, err := m.MarshalBinary()
	require.NoError(t, err)
	back, err := DecodeModule(data)
	require.NoError(t, err)

	got := back.Types[0]
	// Field.Type resolves through the module's type table, so it is a different
	// pointer to the same type; compare it by name and then by value.
	require.Equal(t, inner.Name, got.Fields[0].Type.Name)
	require.Equal(t, inner.Name, got.Cases[0][0].Type.Name)
	got.Fields[0].Type, got.Cases[0][0].Type = inner, inner
	require.Equal(t, agg, got)

	// The layout rule is what the flag is for, so check the consequence too: a
	// packed aggregate that came back unpacked would pad its members.
	wantSize, wantAlign := agg.Layout()
	gotSize, gotAlign := got.Layout()
	require.Equal(t, wantSize, gotSize)
	require.Equal(t, wantAlign, gotAlign)
}

// TestModuleRoundTripsEveryField covers the module itself.
//
// SymAttrs, SymAlign and Aliases were absent from the format entirely, which is
// a different failure from a field the encoder forgot: there was no place to put
// them. What each one is for, and why it is now in the format, is written at the
// call sites in ir/binary.go.
//
// AllocDecisions is deliberately not carried and is skipped here by name rather
// than by silence, so that a reader of this test learns it was a decision.
func TestModuleRoundTripsEveryField(t *testing.T) {
	m := NewModule()
	m.Runtime = true
	m.GoModuleData = "runtime.firstmoduledata"
	m.GoHasMain = true
	m.File("prog.go")
	m.AddType(&AggType{Name: "pair", Fields: []Field{{Sub: SubW}}})
	// Fully populated: the decoder builds empty slices where the encoder was
	// handed nil ones, so a fixture with nil slices compares unequal for a reason
	// that is not a dropped field.
	m.Data = append(m.Data, &Data{
		Name:         "runtime.firstmoduledata",
		Items:        []DataItem{{Sub: SubW, Ints: []int64{1}, Flts: []float64{1.5}, Str: "x"}},
		PointerWords: []int{0},
	})
	m.Aliases = append(m.Aliases,
		&Alias{Name: "memmove", Target: "runtime.memmove", Export: true, Func: true})
	m.SymAlign = map[string]int{"g": 8, "h": 4}
	m.SymAttrs = map[string]SymAttr{
		"runtime.gcWriteBarrier": SymAtomicPointerStore,
		"runtime.deferproc":      SymFrameScoped | SymNoEscape,
	}
	m.Attachments = map[string][]byte{"goc.custom": {0, 1, 2}}
	m.Assembly = append(m.Assembly, AssemblyFile{
		PackagePath:  "runtime",
		Path:         "runtime/asm.s",
		Source:       "TEXT ·f(SB),$0-0\n",
		Defines:      map[string]int64{"NOSPLIT": 4},
		Includes:     map[string]string{"textflag.h": "#define NOSPLIT 4\n"},
		FloatInputs:  map[string][]int{"f": {0}},
		FloatOutputs: map[string][]int{"f": {8}},
		Signatures: map[string]AsmSignature{"f": {
			Params:  []AsmSlot{{Name: "v", Offset: 8, Cls: ClsD, Width: 8, GCRef: true, Group: 1}},
			Results: []AsmSlot{{Name: "r", Offset: 16, Cls: ClsL, Width: 8, GCRef: true, Group: 1}},
		}},
	})
	m.NewFunc("f", ClsW).Entry().RetVoid()
	m.AllocDecisions = nil

	allFieldsSet(t, *m, "AllocDecisions") // diagnostic only; see ir/binary.go
	allFieldsSet(t, *m.Aliases[0])
	allFieldsSet(t, m.Assembly[0])
	allFieldsSet(t, m.Assembly[0].Signatures["f"])
	allFieldsSet(t, m.Assembly[0].Signatures["f"].Params[0])

	data, err := m.MarshalBinary()
	require.NoError(t, err)
	back, err := DecodeModule(data)
	require.NoError(t, err)

	require.Equal(t, m.Runtime, back.Runtime)
	require.Equal(t, m.GoModuleData, back.GoModuleData)
	require.Equal(t, m.GoHasMain, back.GoHasMain)
	require.Equal(t, m.Files, back.Files)
	require.Equal(t, m.Data, back.Data)
	require.Equal(t, m.Aliases, back.Aliases)
	require.Equal(t, m.SymAlign, back.SymAlign)
	require.Equal(t, m.SymAttrs, back.SymAttrs)
	require.Equal(t, m.Attachments, back.Attachments)
	require.Equal(t, m.Assembly, back.Assembly)
	// Funcs and Types are rebuilt, so they are compared by their own round-trip
	// tests; here only that they arrived.
	require.Len(t, back.Funcs, len(m.Funcs))
	require.Len(t, back.Types, len(m.Types))
	require.Equal(t, m.Types[0].Name, back.Types[0].Name)

	// A field that decodes to the zero value is a field the format still drops,
	// whatever the comparisons above happen to cover.
	allFieldsSet(t, *back, "AllocDecisions")
}

// TestBlockRoundTripsEveryField covers a block and its terminator.
//
// Jmp.Likely was the drop this found, and it was not on anyone's list. It is
// __builtin_expect's hint about which edge of a conditional branch is taken, and
// analysis/freq.go reads it to bias the two edges' block frequencies -- which is
// what tells the register allocator to keep hot-edge values in registers and
// spill cold-edge ones. Losing it does not break the branch; it changes the
// register allocation of everything around it.
func TestBlockRoundTripsEveryField(t *testing.T) {
	m := NewModule()
	f := m.NewFunc("f", ClsW)
	f.Param("k", ClsW)
	// Temp 0 would make a Ref with a zero ID, which allFieldsSet cannot tell from
	// an unset one, so the fixture works in temporaries the builder numbered later.
	k := f.NewTemp("j", ClsL)
	b := f.Entry() // first, so it is the function's start block
	yes, no, target := f.NewBlock("yes"), f.NewBlock("no"), f.NewBlock("target")
	yes.Ret(f.Word(1))
	no.Ret(f.Word(2))
	target.Ret(f.Word(3))

	b.Name = "entry"
	b.Sym = "entry.label"
	b.ID = 7
	b.SecondaryEntry = true
	b.SyntheticSuccs = []*Block{target}
	b.Pos = SrcPos{File: 1, Line: 2, Col: 3}
	b.Phis = []*Phi{{Cls: ClsL, To: Ref{Kind: RefTemp, ID: 2}, Args: []Ref{k}, Blocks: []*Block{target}}}
	b.Instrs = []Instr{{Op: OCopy, Cls: ClsL, To: Ref{Kind: RefTemp, ID: 3}, Args: []Ref{k}}}
	f.NewTemp("p", ClsL)
	f.NewTemp("q", ClsL)
	b.Jmp = Jmp{
		Kind:    JmpJnz,
		Arg:     k,
		To:      yes,
		To2:     no,
		Args:    []Ref{k},
		Targets: []*Block{target},
		Cases:   []SwitchCase{{Val: 3, Blk: target}},
		Signed:  true,
		Likely:  LikelyTo2,
	}
	// fn and curPos are builder state, not content: the decoder rebuilds the
	// owner and no encoding of a position-stamping cursor would mean anything.
	// Preds is derived -- the CFG pass fills it from the terminators, which are
	// carried, so re-deriving it is exact.
	allFieldsSet(t, *b, "fn", "curPos", "Preds")
	allFieldsSet(t, b.Jmp)
	allFieldsSet(t, *b.Phis[0])
	allFieldsSet(t, b.Jmp.Cases[0])
	allFieldsSet(t, b.Pos)

	data, err := m.MarshalBinary()
	require.NoError(t, err)
	back, err := DecodeModuleUnverified(data) // the fixture's phi is deliberately ill-formed
	require.NoError(t, err)

	got := back.Funcs[0].Start
	require.Equal(t, b.Name, got.Name)
	require.Equal(t, b.Sym, got.Sym)
	require.Equal(t, b.ID, got.ID)
	require.Equal(t, b.SecondaryEntry, got.SecondaryEntry)
	require.Equal(t, b.Pos, got.Pos)
	// The body is TestInstrRoundTripsEveryField's subject; here only that the block
	// carried it. (An Instr fixture with nil slices compares unequal to a decoded
	// one with empty ones, which is not a dropped field.)
	require.Len(t, got.Instrs, 1)
	require.Equal(t, b.Instrs[0].Op, got.Instrs[0].Op)
	require.Equal(t, b.Instrs[0].To, got.Instrs[0].To)
	require.Equal(t, b.Instrs[0].Args, got.Instrs[0].Args)
	// Block references come back as the decoder's own blocks, so compare by name.
	require.Equal(t, []string{"target"}, blockNames(got.SyntheticSuccs))
	require.Len(t, got.Phis, 1)
	require.Equal(t, b.Phis[0].Cls, got.Phis[0].Cls)
	require.Equal(t, b.Phis[0].To, got.Phis[0].To)
	require.Equal(t, b.Phis[0].Args, got.Phis[0].Args)
	require.Equal(t, []string{"target"}, blockNames(got.Phis[0].Blocks))
	require.Equal(t, b.Jmp.Kind, got.Jmp.Kind)
	require.Equal(t, b.Jmp.Arg, got.Jmp.Arg)
	require.Equal(t, "yes", got.Jmp.To.Name)
	require.Equal(t, "no", got.Jmp.To2.Name)
	require.Equal(t, b.Jmp.Args, got.Jmp.Args)
	require.Equal(t, []string{"target"}, blockNames(got.Jmp.Targets))
	require.Equal(t, b.Jmp.Signed, got.Jmp.Signed)
	require.Equal(t, b.Jmp.Likely, got.Jmp.Likely)
	require.Len(t, got.Jmp.Cases, 1)
	require.Equal(t, b.Jmp.Cases[0].Val, got.Jmp.Cases[0].Val)
	require.Equal(t, "target", got.Jmp.Cases[0].Blk.Name)

	allFieldsSet(t, *got, "fn", "curPos", "Preds")
	allFieldsSet(t, got.Jmp)
}

func blockNames(blocks []*Block) []string {
	names := make([]string, len(blocks))
	for i, b := range blocks {
		names[i] = b.Name
	}
	return names
}

// TestFuncRoundTripsEveryField covers the function record itself.
//
// Two of the drops this found are unexported and so were invisible to the guard
// as it stood -- nameSeq, whose loss stopped the cache path assembling at all,
// and lowered, whose loss disarmed three separate checks. allFieldsSet therefore
// enumerates unexported fields too and makes the exceptions say their name.
func TestFuncRoundTripsEveryField(t *testing.T) {
	m := NewModule()
	pair := &AggType{Name: "pair", Fields: []Field{{Sub: SubL}, {Sub: SubL}}}
	m.AddType(pair)

	f := m.NewFunc("f", ClsL)
	f.Linkage = Linkage{Export: true, Thread: true, Section: ".text.hot", SecArgs: "ax"}
	f.HasRet = true
	f.Retty = ClsL
	f.RetAgg = pair
	f.RetValues = true
	f.Variadic = true
	f.CallConv = CallConvGoInternal
	f.ManagedFrame = true
	f.NoSplit = true
	f.SystemStack = true
	f.HasClosureContext = true
	f.ForceInline = true
	f.CostInline = true
	f.NoInline = true
	f.StackPointerWords = map[uint32]map[int]bool{1: {0: true, 8: true}}
	f.PlacedAllocs = map[uint32]PlacedAlloc{1: {}}
	// A scalar parameter ahead of the group, so the group's Index is not zero and
	// its refs do not name temporary 0 -- allFieldsSet cannot tell either of those
	// from a field nobody set.
	f.Param("k", ClsW)
	parts := f.ParamGroup("value", pair, ClsL, ClsL)
	f.Aggregate(pair, parts...)
	f.Temp(f.NewTemp("closure", ClsP)).ClosureContext = true
	f.Word(1)
	f.Entry().RetAggregate(parts...)
	f.lowered = "arm64"
	f.nameSeq = 12

	allFieldsSet(t, *f,
		"mod",          // the decoder's own module owns the decoded function
		"PlacedAllocs", // diagnostic only, deliberately not carried; see ir/binary.go
		"constIdx",     // derived from Consts and rebuilt on decode, not carried
	)
	allFieldsSet(t, f.ParamGroups[0])
	allFieldsSet(t, f.AggregateValues[0])
	allFieldsSet(t, f.AggregateValues[0].Parts[0])

	data, err := m.MarshalBinary()
	require.NoError(t, err)
	back, err := DecodeModule(data)
	require.NoError(t, err)

	got := back.Funcs[0]
	require.Equal(t, f.Name, got.Name)
	require.Equal(t, f.Linkage, got.Linkage)
	require.Equal(t, f.HasRet, got.HasRet)
	require.Equal(t, f.Retty, got.Retty)
	require.Equal(t, f.RetValues, got.RetValues)
	require.Equal(t, f.Variadic, got.Variadic)
	require.Equal(t, f.CallConv, got.CallConv)
	require.Equal(t, f.ManagedFrame, got.ManagedFrame)
	require.Equal(t, f.NoSplit, got.NoSplit)
	require.Equal(t, f.SystemStack, got.SystemStack)
	require.Equal(t, f.HasClosureContext, got.HasClosureContext)
	require.Equal(t, f.ForceInline, got.ForceInline)
	require.Equal(t, f.CostInline, got.CostInline)
	require.Equal(t, f.NoInline, got.NoInline)
	require.Equal(t, f.StackPointerWords, got.StackPointerWords)
	require.Equal(t, f.LoweredFor(), got.LoweredFor())
	require.Equal(t, f.nameSeq, got.nameSeq)
	require.Equal(t, f.RetAgg.Name, got.RetAgg.Name)
	require.Len(t, got.Params, len(f.Params))
	require.Len(t, got.Temps, len(f.Temps))
	require.Len(t, got.Consts, len(f.Consts))
	require.Len(t, got.Blocks, len(f.Blocks))
	require.Equal(t, f.Start.Name, got.Start.Name)
	require.Len(t, got.ParamGroups, 1)
	require.Equal(t, f.ParamGroups[0].Index, got.ParamGroups[0].Index)
	require.Equal(t, f.ParamGroups[0].Count, got.ParamGroups[0].Count)
	require.Equal(t, f.ParamGroups[0].Type.Name, got.ParamGroups[0].Type.Name)
	require.Len(t, got.AggregateValues, 1)
	require.Equal(t, f.AggregateValues[0].Parts, got.AggregateValues[0].Parts)
	require.Equal(t, f.AggregateValues[0].Type.Name, got.AggregateValues[0].Type.Name)

	// constIdx is not carried, but a decoded function that did not rebuild it
	// appends a second copy of every constant a later pass interns.
	before := len(got.Consts)
	got.Word(1)
	require.Equal(t, before, len(got.Consts),
		"interning a constant the decoded function already holds must find it, not append it")

	allFieldsSet(t, *got, "mod", "PlacedAllocs", "constIdx")
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
		CallConv:          CallConvGoInternal,
		CallConvSet:       true,
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
		ClosureContext:    Ref{Kind: RefTemp, ID: 3},
		Asm: &AsmOp{
			Template:      "movl %k1, %k0",
			Ops:           []AsmOperandKind{AsmRegOut, AsmRegIn},
			ExactClobbers: true,
			Regs:          []string{"", "a"},
			Clobbers:      []string{"cc"},
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
		ID:             0,
		Name:           "x",
		Cls:            ClsL,
		Slot:           8,
		Reg:            3,
		Fixed:          true,
		Agg:            &AggType{Name: "pair", Fields: []Field{{Sub: SubW}}},
		GCRef:          true,
		GCType:         7,
		ClosureContext: true,
	}
	allFieldsSet(t, tmp, "ID") // ID is the index, restored by position

	m := NewModule()
	f := m.NewFunc("f", ClsW)
	f.Temps = []*Temp{&tmp}
	// The fixture marks the temporary as the incoming closure context, so the
	// function has to say it receives one; ir.Verify, which DecodeModule runs,
	// rejects a function that states only half of that.
	f.HasClosureContext = true
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

// formatTypesNotCarried names every struct type reachable from ir.Module that the
// unit format deliberately does not carry, with the reason. A type is on this
// list because somebody decided, not because nobody looked.
var formatTypesNotCarried = map[string]string{
	"AllocDecision": "Module.AllocDecisions is diagnostic only: no pass reads it, and a " +
		"module with it empty compiles to the same code. It exists so the two escape " +
		"analyses can be compared at the same allocation.",
	"PlacedAlloc": "Func.PlacedAllocs is diagnostic only, for the same reason and by the " +
		"same argument as AllocDecision.",
	"constKey": "Func.constIdx's key. The index is derived from Func.Consts and is rebuilt " +
		"on decode rather than carried -- but it must be rebuilt: see decFunc.",
}

// TestEveryTypeTheFormatCarriesIsGuarded is the guard on the guard.
//
// Extending allFieldsSet to a type stops a *field* being added to that type and
// silently dropped. It does nothing about a whole *type* being added to the
// format with no guard at all, which is the same failure one level up -- and the
// format has grown types before.
//
// So the set of struct types the format has to carry is not a list anyone
// maintains: it is computed, by walking ir.Module's type graph. Every type in it
// must either have been through allFieldsSet or be on formatTypesNotCarried with
// a reason.
func TestEveryTypeTheFormatCarriesIsGuarded(t *testing.T) {
	reachable := map[string]bool{}
	var walk func(reflect.Type)
	walk = func(rt reflect.Type) {
		switch rt.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(rt.Elem())
		case reflect.Map:
			walk(rt.Key())
			walk(rt.Elem())
		case reflect.Struct:
			if reachable[rt.Name()] {
				return
			}
			reachable[rt.Name()] = true
			for i := 0; i < rt.NumField(); i++ {
				walk(rt.Field(i).Type)
			}
		}
	}
	walk(reflect.TypeOf(Module{}))

	for name := range reachable {
		if reason, excused := formatTypesNotCarried[name]; excused {
			require.NotEmpty(t, reason)
			continue
		}
		require.Truef(t, guardedTypes[name],
			"ir.%s is reachable from ir.Module, so the unit format has to carry it, and no "+
				"test has put it through allFieldsSet. Write one (see the round-trip tests "+
				"above), or -- if the format is right not to carry it -- add it to "+
				"formatTypesNotCarried with the reason. A type nobody enumerated is a type "+
				"whose fields the encoder can drop one at a time without anything failing.",
			name)
	}

	for name := range formatTypesNotCarried {
		require.Truef(t, reachable[name],
			"ir.%s is excused from the format but is no longer reachable from ir.Module; "+
				"drop the entry", name)
	}
}

// TestGuardedTypesAreReallyGuarded checks the other direction: that guardedTypes
// is a record of what ran and not a list that drifted. It is only meaningful when
// the package ran as a whole, since a -run filter leaves the record empty.
func TestGuardedTypesAreReallyGuarded(t *testing.T) {
	if len(guardedTypes) == 0 {
		t.Log("no allFieldsSet call ran in this invocation; nothing to check")
		return
	}
	for name := range guardedTypes {
		require.NotContainsf(t, formatTypesNotCarried, name,
			"ir.%s is both guarded and excused from the format; one of the two is wrong", name)
	}
}
