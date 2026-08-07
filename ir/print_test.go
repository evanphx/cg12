package ir

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPrintLinkageVariants(t *testing.T) {
	var sb strings.Builder
	printLinkage(&sb, Linkage{Export: true, Thread: true, Section: ".text"})
	assert.Equal(t, "export thread section \".text\" ", sb.String())
}

func TestRefStringVariants(t *testing.T) {
	m := NewModule()
	tr := m.AddType(&AggType{Name: "pt"})
	f := m.NewFuncVoid("f")

	assert.Equal(t, ":pt", f.refString(tr))
	assert.Equal(t, "", f.refString(R))
	assert.Equal(t, "%.slot3", f.refString(slotRef(3)))
	// Type ref past the registered set falls back to a synthetic name.
	assert.Equal(t, ":t9", f.refString(typeRef(9)))
	// A kind with no textual form renders a debug placeholder rather than
	// something that reads like a real operand. 5 is one of the two numbers burned
	// when RefCallArg and RefMem were removed, so nothing will ever name it again.
	assert.Equal(t, "<ref 5:1>", f.refString(Ref{Kind: 5, ID: 1}))
}

func TestConstStringUnknownKind(t *testing.T) {
	f := NewModule().NewFuncVoid("f")
	assert.Equal(t, "?", f.constString(Const{Kind: ConstKind(99)}))
}

func TestPrintJmpHlt(t *testing.T) {
	f := NewModule().NewFuncVoid("f")
	e := f.Entry()
	e.Hlt()
	assert.Contains(t, f.String(), "\thlt\n")
	assert.Equal(t, JmpHlt, e.Jmp.Kind)
}

func TestPrintRetAggregate(t *testing.T) {
	m := NewModule()
	agg := &AggType{Name: "pair", Fields: []Field{{Sub: SubW}, {Sub: SubW}}}
	m.AddType(agg)
	f := m.NewFunc("mk", ClsW)
	f.RetAgg = agg // returns aggregate by value
	f.Entry().RetVoid()
	assert.Contains(t, f.String(), "function :pair $mk() {")
}

func TestPrintDataFloats(t *testing.T) {
	var sb strings.Builder
	printData(&sb, &Data{Name: "fs", Items: []DataItem{
		{Sub: SubD, Flts: []float64{1.5, 2.25}},
	}})
	assert.Equal(t, "data $fs = { d 1.5 2.25 }\n", sb.String())
}

func TestPrintAggregates(t *testing.T) {
	m := NewModule()
	pair := &AggType{Name: "pair", Fields: []Field{{Sub: SubW}, {Sub: SubW}}}
	m.AddType(pair)
	f := m.NewFuncVoid("f")
	// aggregate parameter
	pr := f.NewTemp("p", ClsL)
	f.Temp(pr).Agg = pair
	f.Params = append(f.Params, f.Temp(pr))
	e := f.Entry()
	// a call with an aggregate argument and an aggregate result
	r := e.Call(ClsL, f.Sym("g", 0), pr)
	ci := &e.Instrs[len(e.Instrs)-1]
	ci.SetAggArgs([]*AggType{pair})
	ci.RetAgg = pair
	_ = r
	e.RetVoid()

	s := f.String()
	assert.Contains(t, s, "function $f(:pair %p)", "aggregate parameter")
	assert.Contains(t, s, "=:pair call $g(:pair %p)", "aggregate result and argument")
}

func TestModulePrintMultipleTypes(t *testing.T) {
	m := NewModule()
	m.AddType(&AggType{Name: "a", Fields: []Field{{Sub: SubW}}})
	m.AddType(&AggType{Name: "b", Fields: []Field{{Sub: SubL}}})
	s := m.String()
	// Consecutive type definitions are separated by a newline.
	assert.Contains(t, s, "type :a = { w }\n\ntype :b = { l }\n")
}

func TestModulePrintFuncsOnly(t *testing.T) {
	m := NewModule()
	f1 := m.NewFuncVoid("a")
	f1.Entry().RetVoid()
	f2 := m.NewFuncVoid("b")
	f2.Entry().RetVoid()
	s := m.String()
	// A blank line separates consecutive functions.
	assert.Contains(t, s, "}\n\nfunction $b()")
}
