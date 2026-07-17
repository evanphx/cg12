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
