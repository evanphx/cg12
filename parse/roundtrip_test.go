package parse_test

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// roundTrip prints a module, parses it back, prints again, and asserts the two
// texts are identical.
func roundTrip(t *testing.T, m *ir.Module) {
	t.Helper()
	first := m.String()
	m2, err := parse.Parse(first)
	require.NoErrorf(t, err, "parsing:\n%s", first)
	second := m2.String()
	assert.Equal(t, first, second, "round trip diverged")
}

func TestRoundTripAdd(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("add", ir.ClsW).Export()
	a, b := f.Param("a", ir.ClsW), f.Param("b", ir.ClsW)
	e := f.Entry()
	e.Ret(e.Add(ir.ClsW, a, b))
	roundTrip(t, m)
}

func TestRoundTripIntrinsic(t *testing.T) {
	// Both intrinsic forms survive a print/parse/print cycle: the value-producing
	// stacksave (names a result) and the void stackrestore (takes an operand).
	m := ir.NewModule()
	f := m.NewFuncVoid("f").Export()
	e := f.Entry()
	sp := e.StackSave()
	e.StackRestore(sp)
	e.RetVoid()
	roundTrip(t, m)

	// The parsed form has the payload the backends and optimizer read.
	m2, err := parse.Parse(m.String())
	require.NoError(t, err)
	in := m2.Funcs[0].Start.Instrs
	require.Len(t, in, 2)
	assert.Equal(t, "stacksave", in[0].Intrin.Name)
	assert.Equal(t, "stackrestore", in[1].Intrin.Name)
}

func TestRoundTripInlineAssembly(t *testing.T) {
	module := ir.NewModule()
	function := module.NewFunc("addOne", ir.ClsW).Export()
	input := function.Param("input", ir.ClsW)
	entry := function.Entry()
	outputs := entry.Asm("add %w0, %w1, #1", []ir.AsmSpec{
		{Kind: ir.AsmRegOut, Cls: ir.ClsW},
		{Kind: ir.AsmRegIn, Ref: input},
	})
	instruction := &entry.Instrs[len(entry.Instrs)-1]
	instruction.Asm.ExactClobbers = true
	instruction.Asm.Clobbers = []string{"cc", "memory"}
	entry.Ret(outputs[0])

	roundTrip(t, module)

	parsed, err := parse.Parse(module.String())
	require.NoError(t, err)
	assembly := parsed.Funcs[0].Entry().Instrs[0]
	require.NotNil(t, assembly.Asm)
	assert.Equal(t, "add %w0, %w1, #1", assembly.Asm.Template)
	assert.Equal(t, []ir.AsmOperandKind{ir.AsmRegOut, ir.AsmRegIn}, assembly.Asm.Ops)
	assert.True(t, assembly.Asm.ExactClobbers)
	assert.Equal(t, []string{"cc", "memory"}, assembly.Asm.Clobbers)
}

func TestRoundTripAtomics(t *testing.T) {
	// Atomic intrinsics carry the operation and width in a dotted name, and a
	// store is void; both survive print/parse/print.
	m := ir.NewModule()
	f := m.NewFuncVoid("f").Export()
	p := f.Param("p", ir.ClsL)
	e := f.Entry()
	e.AtomicStore(ir.ClsL, p, f.Long(1))
	e.AtomicRMW("add", ir.ClsL, p, f.Long(2))
	e.AtomicFence()
	e.RetVoid()
	roundTrip(t, m)

	m2, err := parse.Parse(m.String())
	require.NoError(t, err)
	in := m2.Funcs[0].Start.Instrs
	assert.Equal(t, "atomic.store.l", in[0].Intrin.Name)
	assert.Equal(t, "atomic.add.l", in[1].Intrin.Name)
	assert.Equal(t, "atomic.fence", in[2].Intrin.Name)
}

func TestRoundTripLoopWithPhis(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("sumto", ir.ClsW).Export()
	n := f.Param("n", ir.ClsW)
	start := f.Entry()
	loop := f.NewBlock("loop")
	body := f.NewBlock("body")
	exit := f.NewBlock("exit")
	start.Goto(loop)
	i := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	s := loop.Phi(ir.ClsW, ir.PhiEdge{From: start, Val: f.Word(0)})
	loop.Jnz(loop.Cmp(ir.CmpSge, ir.ClsW, i, n), exit, body)
	s2 := body.Add(ir.ClsW, s, i)
	i2 := body.Add(ir.ClsW, i, f.Word(1))
	body.Goto(loop)
	loop.Phis[0].Add(body, i2)
	loop.Phis[1].Add(body, s2)
	exit.Ret(s)
	roundTrip(t, m)
}

func TestRoundTripBranchesOutOfOrderTargets(t *testing.T) {
	// jnz names its false target before the true one appears; block order must
	// still follow label definitions.
	m := ir.NewModule()
	f := m.NewFunc("f", ir.ClsW)
	c := f.Param("c", ir.ClsW)
	start := f.Entry()
	a := f.NewBlock("a")
	b := f.NewBlock("b")
	start.Jnz(c, b, a) // b referenced before a's label
	a.Ret(f.Word(1))
	b.Ret(f.Word(2))
	roundTrip(t, m)
}

func TestRoundTripCallsAndMemory(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("g", ir.ClsL).Export()
	x := f.Param("x", ir.ClsL)
	e := f.Entry()
	p := e.Alloc(8, 8)
	e.Store(x, p)
	r := e.Call(ir.ClsL, f.Sym("h", 0), e.Load(ir.ClsL, p), f.Long(3))
	e.CallVoid(f.Sym("side", 0))
	e.Ret(r)
	roundTrip(t, m)
}

func TestRoundTripFloats(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("fx", ir.ClsD).Export()
	a := f.Param("a", ir.ClsD)
	b := f.Param("b", ir.ClsS)
	e := f.Entry()
	c := e.Cmp(ir.CmpFlt, ir.ClsW, a, e.Exts(b))
	e.Ret(e.Add(ir.ClsD, a, e.Sltof(ir.ClsD, e.Extsw(ir.ClsL, c))))
	e.Ret(e.Add(ir.ClsD, a, f.Double(1.5)))
	roundTrip(t, m)
}

func TestRoundTripAggregates(t *testing.T) {
	// Aggregate parameters, call arguments, and returns.
	m, err := parse.Parse(`
type :pt = { w, w }
type :big = { l, l, l }

function :pt $mk(w %x) {
@start
	%p =l alloc8 8
	ret %p
}

export
function w $use(:pt %a, :big %b) {
@start
	%r =w call $sink(w 1, :pt %a, :big %b)
	ret %r
}
`)
	require.NoError(t, err)
	assert.Equal(t, m.String(), reparse(t, m.String()))
	// Structural checks.
	use := m.Funcs[1]
	require.NotNil(t, use.Params[0].Agg)
	assert.Equal(t, "pt", use.Params[0].Agg.Name)
	call := use.Blocks[0].Instrs[0]
	require.Len(t, call.AggArgs(), 3)
	assert.Nil(t, call.AggArgs()[0])
	assert.Equal(t, "pt", call.AggArgs()[1].Name)
	assert.Equal(t, "big", call.AggArgs()[2].Name)
}

func reparse(t *testing.T, src string) string {
	m, err := parse.Parse(src)
	require.NoError(t, err)
	return m.String()
}

func TestRoundTripUnion(t *testing.T) {
	m, err := parse.Parse(`
type :u = { { b } { s } }
type :s9 = { w, :u }

export function w $f(:s9 %p) {
@start
	ret 0
}
`)
	require.NoError(t, err)
	assert.Equal(t, m.String(), reparse(t, m.String()))
	u := m.Types[0]
	require.True(t, u.Union)
	require.Len(t, u.Cases, 2)
	// { b } is 1 byte, { s } is 4 -> union is 4 bytes, align 4.
	sz, al := u.Layout()
	assert.Equal(t, 4, sz)
	assert.Equal(t, 4, al)
}

func TestRoundTripDataAndTypes(t *testing.T) {
	m := ir.NewModule()
	m.Types = append(m.Types, &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubL}}})
	m.Types = append(m.Types, &ir.AggType{Name: "opaque", Opaque: true, Size: 16, Align: 8})
	m.Data = append(m.Data, &ir.Data{
		Name:    "nums",
		Linkage: ir.Linkage{Export: true},
		Align:   4,
		Items: []ir.DataItem{
			{Sub: ir.SubW, Ints: []int64{1, 2, 3}},
			{Zero: 8},
			{Sub: ir.SubL, Sym: "other", Off: 16},
		},
	})
	f := m.NewFuncVoid("noop")
	f.Entry().RetVoid()
	roundTrip(t, m)
}
