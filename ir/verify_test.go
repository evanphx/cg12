package ir

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The IR has three front doors -- the builder, the text parser, the binary
// decoder -- and they did not agree about what the IR is. The builder can only
// make a well-formed OAsm, because Block.Asm takes the template. The parser
// would make one from the word "asm" alone. Nothing said which was right, so the
// backend's nil dereference was the only answer.
func TestVerifyCatchesAMissingPayload(t *testing.T) {
	f := newFuncWith(func(f *Func, b *Block) {
		b.Instrs = append(b.Instrs, Instr{Op: OAsm, Cls: ClsW, To: f.NewTemp("r", ClsW)})
	})
	require.ErrorContains(t, Verify(f), "no template")

	g := newFuncWith(func(f *Func, b *Block) {
		b.Instrs = append(b.Instrs, Instr{Op: OBlockAddr, Cls: ClsL, To: f.NewTemp("a", ClsL)})
	})
	require.ErrorContains(t, Verify(g), "no target block")
}

// A reference to a temporary that does not exist indexes out of range in
// whichever pass reaches it first.
func TestVerifyCatchesADanglingRef(t *testing.T) {
	f := newFuncWith(func(f *Func, b *Block) {
		b.Instrs = append(b.Instrs, Instr{
			Op: OAdd, Cls: ClsW, To: f.NewTemp("r", ClsW),
			Args: []Ref{{Kind: RefTemp, ID: 99}, {Kind: RefTemp, ID: 0}},
		})
	})
	require.ErrorContains(t, Verify(f), "%99 does not exist")
}

// A block with no terminator is one under construction, which is not a function.
func TestVerifyCatchesAnUnterminatedBlock(t *testing.T) {
	m := NewModule()
	f := m.NewFunc("f", ClsW)
	f.Entry() // never terminated
	require.ErrorContains(t, Verify(f), "no terminator")
}

// A phi whose arguments and predecessors disagree cannot be read at all.
func TestVerifyCatchesAMismatchedPhi(t *testing.T) {
	m := NewModule()
	f := m.NewFunc("f", ClsW)
	e := f.Entry()
	b := f.NewBlock("b")
	e.Goto(b)
	b.Phis = append(b.Phis, &Phi{Cls: ClsW, To: f.NewTemp("p", ClsW),
		Args: []Ref{f.Word(1), f.Word(2)}, Blocks: []*Block{e}})
	b.Ret(f.Word(0))
	require.ErrorContains(t, Verify(f), "2 arguments for 1 predecessors")
}

// A temporary that exists in Temps but is never assigned passes the bounds check
// yet is undefined -- the parser mints one on first mention, so a dropped
// definition survives as a read of nothing, on which a dominance-frontier pass can
// loop. verifyRef cannot see it; the def/use pass must.
func TestVerifyCatchesAnUndefinedTemp(t *testing.T) {
	f := newFuncWith(func(f *Func, b *Block) {
		x := f.NewTemp("x", ClsW) // in Temps, but nothing ever assigns it
		b.Instrs = append(b.Instrs, Instr{Op: OCopy, Cls: ClsW, To: f.NewTemp("r", ClsW), Args: []Ref{x}})
	})
	require.ErrorContains(t, Verify(f), "which nothing defines")
}

// A temporary assigned twice is not in SSA form; the passes that assume one
// definition per name would read the wrong one.
func TestVerifyCatchesADoubleAssignment(t *testing.T) {
	f := newFuncWith(func(f *Func, b *Block) {
		r := f.NewTemp("r", ClsW)
		b.Instrs = append(b.Instrs,
			Instr{Op: OCopy, Cls: ClsW, To: r, Args: []Ref{f.Word(1)}},
			Instr{Op: OCopy, Cls: ClsW, To: r, Args: []Ref{f.Word(2)}},
		)
	})
	require.ErrorContains(t, Verify(f), "assigned more than once")
}

// A phi naming an incoming block that does not branch to it encodes a premise the
// control flow contradicts -- the argument-count check passes, but the edge is a
// fiction.
func TestVerifyCatchesAPhiFromANonPredecessor(t *testing.T) {
	m := NewModule()
	f := m.NewFunc("f", ClsW)
	e := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	e.Jnz(f.Word(1), a, b) // e -> a, e -> b; a does not branch to b
	a.Ret(f.Word(0))
	p := f.NewTemp("p", ClsW)
	b.Phis = append(b.Phis, &Phi{Cls: ClsW, To: p, Args: []Ref{f.Word(3)}, Blocks: []*Block{a}})
	b.Ret(p)
	require.ErrorContains(t, Verify(f), "does not branch here")
}

// A closure's environment arrives in the dedicated closure register, so no
// instruction assigns it and it is not a parameter -- and reading it is not a
// use before definition. Every closure, deferwrap, gowrap and methodvalue goc
// emits that touches a captured variable has this shape, and the verifier used
// to reject all of them: 4-6% of the functions in a whole-program compile.
func TestVerifyAcceptsTheIncomingClosureContext(t *testing.T) {
	f := newFuncWith(func(f *Func, b *Block) {
		f.HasClosureContext = true
		context := f.NewTemp("closure", ClsP)
		f.Temp(context).ClosureContext = true
		// What a capture read looks like: environment + offset, then a load.
		address := f.NewTemp("capture.addr", ClsL)
		b.Instrs = append(b.Instrs, Instr{Op: OAdd, Cls: ClsL, To: address, Args: []Ref{context, f.Long(8)}})
	})
	require.NoError(t, Verify(f))
}

// The exemption is only for the temporary the function says receives the
// context. An ordinary temporary nothing assigns is still a use before
// definition, even in a function that does have a closure context -- otherwise
// the fix for the closure case would have switched the check off for them.
func TestVerifyStillCatchesAnUndefinedTempAlongsideAClosureContext(t *testing.T) {
	f := newFuncWith(func(f *Func, b *Block) {
		f.HasClosureContext = true
		context := f.NewTemp("closure", ClsP)
		f.Temp(context).ClosureContext = true
		stray := f.NewTemp("stray", ClsL) // in Temps, assigned by nothing
		b.Instrs = append(b.Instrs, Instr{Op: OAdd, Cls: ClsL, To: f.NewTemp("r", ClsL), Args: []Ref{context, stray}})
	})
	require.ErrorContains(t, Verify(f), "%1, which nothing defines")
}

// The flag and the marked temporary are two halves of one fact. If they can
// disagree, "the ABI defines this one on entry" stops being checkable: a
// temporary could claim the exemption in a function that receives no context,
// or a function could claim a context that no temporary receives.
func TestVerifyCatchesAnInconsistentClosureContext(t *testing.T) {
	unflagged := newFuncWith(func(f *Func, b *Block) {
		context := f.NewTemp("closure", ClsP)
		f.Temp(context).ClosureContext = true // but HasClosureContext is not set
		b.Instrs = append(b.Instrs, Instr{Op: OCopy, Cls: ClsP, To: f.NewTemp("r", ClsP), Args: []Ref{context}})
	})
	require.ErrorContains(t, Verify(unflagged), "HasClosureContext is not set")

	missing := newFuncWith(func(f *Func, b *Block) {
		f.HasClosureContext = true // but no temporary is marked
	})
	require.ErrorContains(t, Verify(missing), "no temporary is marked")

	two := newFuncWith(func(f *Func, b *Block) {
		f.HasClosureContext = true
		first := f.NewTemp("closure", ClsP)
		f.Temp(first).ClosureContext = true
		second := f.NewTemp("closure.also", ClsP)
		f.Temp(second).ClosureContext = true
		b.Instrs = append(b.Instrs, Instr{Op: OAdd, Cls: ClsL, To: f.NewTemp("r", ClsL), Args: []Ref{first, second}})
	})
	require.ErrorContains(t, Verify(two), "at most one")
}

// What the builder makes is well-formed, so verifying costs nothing real.
func TestVerifyAcceptsAWellFormedFunction(t *testing.T) {
	m := NewModule()
	f := m.NewFunc("f", ClsW)
	a, b := f.Param("a", ClsW), f.Param("b", ClsW)
	e := f.Entry()
	loop := f.NewBlock("loop")
	e.Goto(loop)
	p := loop.Phi(ClsW, PhiEdge{From: e, Val: a})
	loop.Phis[0].Add(loop, b)
	loop.Jnz(loop.Cmp(CmpNe, ClsW, p, b), loop, e)
	require.NoError(t, Verify(f))
	require.NoError(t, VerifyModule(m))
}

// newFuncWith builds a one-block function via the given body, terminated.
func newFuncWith(body func(*Func, *Block)) *Func {
	m := NewModule()
	f := m.NewFunc("f", ClsW)
	b := f.Entry()
	body(f, b)
	b.Ret(f.Word(0))
	return f
}

// Leaves and Layout must describe the same aggregate: a leaf past the end, or a
// size that does not cover the leaves, means the placement rule and the size
// rule have drifted apart -- which is exactly what happened when the ABI
// classifiers each had their own copy of it.
func TestLeavesAgreeWithLayout(t *testing.T) {
	i32 := Field{Sub: SubW}
	i8 := Field{Sub: SubB}
	f64 := Field{Sub: SubD}
	inner := &AggType{Name: "inner", Fields: []Field{i8, i32}}

	for _, c := range []struct {
		name  string
		agg   *AggType
		want  []Leaf // expected leaves, in order
		size  int
		align int
	}{
		{"scalars", &AggType{Fields: []Field{i32, i32}},
			[]Leaf{{SubW, 0}, {SubW, 4}}, 8, 4},
		{"padding", &AggType{Fields: []Field{i8, i32}},
			[]Leaf{{SubB, 0}, {SubW, 4}}, 8, 4}, // the int aligns past the byte
		{"trailing pad", &AggType{Fields: []Field{i32, i8}},
			[]Leaf{{SubW, 0}, {SubB, 4}}, 8, 4}, // rounded up to the alignment
		{"array", &AggType{Fields: []Field{{Sub: SubW, Count: 3}}},
			[]Leaf{{SubW, 0}, {SubW, 4}, {SubW, 8}}, 12, 4},
		{"nested", &AggType{Fields: []Field{i32, {Type: inner}}},
			[]Leaf{{SubW, 0}, {SubB, 4}, {SubW, 8}}, 12, 4},
		{"nested array", &AggType{Fields: []Field{{Type: inner, Count: 2}}},
			[]Leaf{{SubB, 0}, {SubW, 4}, {SubB, 8}, {SubW, 12}}, 16, 4},
		{"doubles", &AggType{Fields: []Field{f64, f64}},
			[]Leaf{{SubD, 0}, {SubD, 8}}, 16, 8},
	} {
		t.Run(c.name, func(t *testing.T) {
			leaves, simple := c.agg.Leaves()
			require.True(t, simple, "no union or opaque type here")
			require.Equal(t, c.want, leaves)

			size, align := c.agg.Layout()
			require.Equal(t, c.size, size)
			require.Equal(t, c.align, align)
			for _, l := range leaves {
				require.LessOrEqualf(t, l.Off+l.Sub.Size(), size,
					"leaf at %d runs past the aggregate's %d bytes", l.Off, size)
			}
		})
	}
}

// A union's cases overlap: every case begins at the same offset. That is what a
// union is, and a classifier has to see it -- so the leaves are reported, and
// simple says they cannot be read as a flat sequence.
func TestLeavesOfAUnionOverlap(t *testing.T) {
	u := &AggType{Name: "u", Union: true, Cases: [][]Field{
		{{Sub: SubW}},
		{{Sub: SubD}},
	}}
	leaves, simple := u.Leaves()
	require.False(t, simple, "a union's leaves are not a flat sequence")
	require.Equal(t, []Leaf{{SubW, 0}, {SubD, 0}}, leaves, "both cases start at 0")

	size, align := u.Layout()
	require.Equal(t, 8, size, "the largest case")
	require.Equal(t, 8, align)

	// And a union nested inside a struct makes the whole thing non-simple.
	s := &AggType{Fields: []Field{{Sub: SubW}, {Type: u}}}
	_, simple = s.Leaves()
	require.False(t, simple, "a union anywhere inside is still a union")
}

// An opaque type's members are unknown by construction.
func TestLeavesOfAnOpaqueType(t *testing.T) {
	o := &AggType{Name: "o", Opaque: true, Size: 16, Align: 8}
	leaves, simple := o.Leaves()
	require.False(t, simple)
	require.Empty(t, leaves)
	size, align := o.Layout()
	require.Equal(t, 16, size)
	require.Equal(t, 8, align)
}
