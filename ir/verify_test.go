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
