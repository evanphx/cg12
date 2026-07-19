package arm64

import (
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/assert"
)

// aggParam adds an aggregate-by-value parameter of type agg to f and returns its
// pointer ref, mirroring what the parser does for ":type %p".
func aggParam(f *ir.Func, name string, agg *ir.AggType) ir.Ref {
	r := f.NewTemp(name, ir.ClsL)
	t := f.Temp(r)
	t.Agg = agg
	f.Params = append(f.Params, t)
	return r
}

func TestCompileAggregateParamGP(t *testing.T) {
	m := ir.NewModule()
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m.AddType(pair)
	f := m.NewFunc("f", ir.ClsW)
	p := aggParam(f, "p", pair)
	e := f.Entry()
	e.Ret(e.Load(ir.ClsW, p))

	asm := disasmModule(t, m)
	// The struct arrives in x0 and is stored into a reconstructed stack slot.
	assert.Contains(t, asm, "add x", "slot address computed from x29")
	assert.Contains(t, asm, "str x0")
}

func TestCompileAggregateParamHFA(t *testing.T) {
	m := ir.NewModule()
	vec := &ir.AggType{Name: "vec2", Fields: []ir.Field{{Sub: ir.SubS}, {Sub: ir.SubS}}}
	m.AddType(vec)
	f := m.NewFunc("f", ir.ClsS)
	p := aggParam(f, "p", vec)
	e := f.Entry()
	e.Ret(e.Load(ir.ClsS, p))

	asm := disasmModule(t, m)
	// HFA elements arrive in s0/s1 and are stored into the slot.
	assert.Contains(t, asm, "str s0")
	assert.Contains(t, asm, "str s1")
}

func TestCompileAggregateArg(t *testing.T) {
	m := ir.NewModule()
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m.AddType(pair)
	f := m.NewFunc("caller", ir.ClsW)
	e := f.Entry()
	ptr := e.Alloc(8, 8)
	r := e.Call(ir.ClsW, f.Sym("sink", 0), ptr)
	call := &e.Instrs[len(e.Instrs)-1]
	call.AggArgs = []*ir.AggType{pair} // the single arg is a by-value aggregate
	e.Ret(r)

	asm := disasmModule(t, m)
	// The aggregate is loaded into x0 and passed by register.
	assert.Contains(t, asm, "ldr")
	assert.Contains(t, asm, "bl sink")
}

func TestCompileAggregateArgHFA(t *testing.T) {
	m := ir.NewModule()
	vec := &ir.AggType{Name: "vec2", Fields: []ir.Field{{Sub: ir.SubS}, {Sub: ir.SubS}}}
	m.AddType(vec)
	f := m.NewFuncVoid("caller")
	e := f.Entry()
	ptr := e.Alloc(8, 8)
	e.CallVoid(f.Sym("sink", 0), ptr)
	call := &e.Instrs[len(e.Instrs)-1]
	call.AggArgs = []*ir.AggType{vec}
	e.RetVoid()

	asm := disasmModule(t, m)
	assert.Contains(t, asm, "ldr s") // HFA elements loaded into SIMD registers
	assert.Contains(t, asm, "bl sink")
}

func TestCompileAggregateArgMemoryOnStack(t *testing.T) {
	// Eight scalar ints fill x0..x7; a >16-byte aggregate is passed by reference,
	// its pointer going to the outgoing stack area (a scalar stack arg).
	m := ir.NewModule()
	big := &ir.AggType{Name: "big", Fields: []ir.Field{{Sub: ir.SubB, Count: 24}}}
	m.AddType(big)
	f := m.NewFuncVoid("caller")
	e := f.Entry()
	ptr := e.Alloc(16, 24)
	args := []ir.Ref{f.Word(0), f.Word(1), f.Word(2), f.Word(3), f.Word(4), f.Word(5), f.Word(6), f.Word(7), ptr}
	e.CallVoid(f.Sym("sink", 0), args...)
	call := &e.Instrs[len(e.Instrs)-1]
	call.AggArgs = make([]*ir.AggType, 9)
	call.AggArgs[8] = big
	e.RetVoid()

	asm := disasmModule(t, m)
	assert.Contains(t, asm, "sub sp", "outgoing stack area reserved")
}

func TestCompileAggregateArgStacked(t *testing.T) {
	// A GP aggregate that doesn't fit in the remaining integer registers is passed
	// on the stack: with x0..x7 taken by the eight word arguments, the pair spills
	// to the outgoing stack area. The caller reserves that area and stores the
	// aggregate's words into it.
	m := ir.NewModule()
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m.AddType(pair)
	f := m.NewFuncVoid("caller")
	e := f.Entry()
	ptr := e.Alloc(8, 8)
	args := []ir.Ref{f.Word(0), f.Word(1), f.Word(2), f.Word(3), f.Word(4), f.Word(5), f.Word(6), f.Word(7), ptr}
	e.CallVoid(f.Sym("sink", 0), args...)
	call := &e.Instrs[len(e.Instrs)-1]
	call.AggArgs = make([]*ir.AggType, 9)
	call.AggArgs[8] = pair // all x0..x7 used -> the aggregate is stacked
	e.RetVoid()

	asm := disasmModule(t, m) // compiles cleanly and reads back the machine code
	assert.Contains(t, asm, "sub sp", "the outgoing stack-argument area is reserved")
	assert.Contains(t, asm, "[sp", "the aggregate's words are stored into it")
}

func TestCompileAggregateByReference(t *testing.T) {
	m := ir.NewModule()
	big := &ir.AggType{Name: "big", Fields: []ir.Field{{Sub: ir.SubB, Count: 24}}}
	m.AddType(big)
	f := m.NewFunc("f", ir.ClsW)
	p := aggParam(f, "p", big) // > 16 bytes: passed as a pointer
	e := f.Entry()
	e.Ret(e.Load(ir.ClsW, p))

	asm := disasmModule(t, m)
	// No reconstruction stores for a by-reference aggregate; the pointer is used.
	assert.NotContains(t, asm, "str x0")
}

func TestCompileStackedAggregateParam(t *testing.T) {
	// Eight scalar ints fill x0..x7; a following GP aggregate is passed on the
	// stack, and the parameter is the address of the incoming bytes.
	m := ir.NewModule()
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m.AddType(pair)
	f := m.NewFunc("f", ir.ClsW)
	for i := 0; i < 8; i++ {
		f.Param("i", ir.ClsW)
	}
	p := aggParam(f, "p", pair)
	e := f.Entry()
	e.Ret(e.Load(ir.ClsW, p))

	asm := disasmModule(t, m)
	assert.Contains(t, asm, "add x", "stacked aggregate parameter address computed from x29")
}

// aggReturnFunc builds a function returning agg by value: it allocs a buffer,
// and returns the pointer.
func aggReturnFunc(m *ir.Module, name string, agg *ir.AggType) *ir.Func {
	f := m.NewFuncVoid(name)
	f.HasRet = true
	f.RetAgg = agg
	e := f.Entry()
	sz, _ := agg.Layout()
	p := e.Alloc(16, sz)
	e.Ret(p)
	return f
}

func TestCompileAggregateReturnGP(t *testing.T) {
	m := ir.NewModule()
	pair := &ir.AggType{Name: "pair", Fields: []ir.Field{{Sub: ir.SubW}, {Sub: ir.SubW}}}
	m.AddType(pair)
	aggReturnFunc(m, "f", pair)
	asm := disasmModule(t, m)
	// The GP result ends up in x0 -- now loaded straight into it, since copy
	// coalescing folds away the move the return once needed. "x0," is x0 as a
	// destination (first operand).
	assert.Contains(t, asm, "x0,", "GP result ends up in x0")
}

func TestCompileAggregateReturnHFA(t *testing.T) {
	m := ir.NewModule()
	vec := &ir.AggType{Name: "vec3", Fields: []ir.Field{{Sub: ir.SubS}, {Sub: ir.SubS}, {Sub: ir.SubS}}}
	m.AddType(vec)
	aggReturnFunc(m, "f", vec)
	asm := disasmModule(t, m)
	// Three HFA elements end up in s0/s1/s2 -- loaded straight into them now that
	// copy coalescing folds away the moves. "s0,"/"s2," is the register as a
	// destination (first operand).
	assert.Contains(t, asm, "s0,")
	assert.Contains(t, asm, "s2,")
}

func TestCompileAggregateReturnMemory(t *testing.T) {
	m := ir.NewModule()
	big := &ir.AggType{Name: "big", Fields: []ir.Field{{Sub: ir.SubB, Count: 20}}}
	m.AddType(big)
	aggReturnFunc(m, "f", big)
	asm := disasmModule(t, m)
	// The 20-byte result is copied into the caller's x8 buffer (16 + 4 bytes).
	assert.Contains(t, asm, "ldr x")
	assert.Contains(t, asm, "str w") // the trailing 4-byte chunk
}

func TestCompileAggregateResultMemory(t *testing.T) {
	// A cg12 caller receiving a > 16-byte aggregate result allocates a buffer and
	// passes it in x8.
	m := ir.NewModule()
	big := &ir.AggType{Name: "big", Fields: []ir.Field{{Sub: ir.SubB, Count: 24}}}
	m.AddType(big)
	f := m.NewFunc("caller", ir.ClsL)
	e := f.Entry()
	r := e.Call(ir.ClsL, f.Sym("mk", 0))
	call := &e.Instrs[len(e.Instrs)-1]
	call.RetAgg = big
	e.Ret(e.Load(ir.ClsL, r))

	asm := disasmModule(t, m)
	assert.Contains(t, asm, "x8", "buffer address passed in x8")
}

func TestHFAClassificationRejects(t *testing.T) {
	// A widening sub-type (uh) is not a valid HFA element.
	_, _, ok := hfaOf(&ir.AggType{Fields: []ir.Field{{Sub: ir.SubUH}, {Sub: ir.SubUH}}})
	assert.False(t, ok)
	// An empty aggregate is not an HFA.
	_, _, ok = hfaOf(&ir.AggType{})
	assert.False(t, ok)
	// A nested HFA flattens.
	inner := &ir.AggType{Fields: []ir.Field{{Sub: ir.SubD}}}
	elem, n, ok := hfaOf(&ir.AggType{Fields: []ir.Field{{Type: inner}, {Sub: ir.SubD}}})
	assert.True(t, ok)
	assert.Equal(t, ir.ClsD, elem)
	assert.Equal(t, 2, n)
	// Opaque aggregates are never HFAs.
	_, _, ok = hfaOf(&ir.AggType{Opaque: true, Size: 8})
	assert.False(t, ok)
}

func TestStoreLoadOpFor(t *testing.T) {
	assert.Equal(t, ir.OStorel, storeOpFor(ir.ClsL))
	assert.Equal(t, ir.OStores, storeOpFor(ir.ClsS))
	assert.Equal(t, ir.OStored, storeOpFor(ir.ClsD))
	assert.Equal(t, ir.OStorew, storeOpFor(ir.ClsW))
	assert.Equal(t, ir.OLoadl, loadOpFor(ir.ClsL))
	assert.Equal(t, ir.OLoads, loadOpFor(ir.ClsS))
	assert.Equal(t, ir.OLoadd, loadOpFor(ir.ClsD))
	assert.Equal(t, ir.OLoaduw, loadOpFor(ir.ClsW))
}
