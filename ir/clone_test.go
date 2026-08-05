package ir

import (
	"testing"
)

// sampleFunc builds a function with a call, a stack allocation, a branch and a
// phi, so the round trip has something to lose.
func sampleFunc() (*Module, *Func) {
	m := NewModule()
	file := m.File("prog.go")

	callee := m.NewFunc("callee", ClsL)
	callee.Entry().Ret(callee.ConstInt(ClsL, 7))

	f := m.NewFunc("caller", ClsL)
	f.ManagedFrame = true
	f.NoSplit = true
	parameter := f.Param("n", ClsL)
	entry := f.Entry()
	entry.At(SrcPos{File: file, Line: 12, Col: 3})
	slot := entry.Alloc(8, 64)
	entry.Store(parameter, slot)
	value := entry.Call(ClsL, f.Sym("callee", 0))
	low, high, end := f.NewBlock("low"), f.NewBlock("high"), f.NewBlock("end")
	entry.Jnz(entry.Cmp(CmpSgt, ClsL, value, parameter), low, high)
	low.Goto(end)
	high.Goto(end)
	end.Ret(end.Phi(ClsL, PhiEdge{From: low, Val: value}, PhiEdge{From: high, Val: parameter}))
	return m, f
}

func TestCloneFuncProducesAnIndependentCopy(t *testing.T) {
	_, f := sampleFunc()
	before := f.String()

	clone, err := CloneFunc(f)
	if err != nil {
		t.Fatalf("CloneFunc: %v", err)
	}
	if clone == f {
		t.Fatal("CloneFunc returned the original")
	}
	if got := clone.String(); got != before {
		t.Errorf("clone differs from the original:\n--- original\n%s\n--- clone\n%s", before, got)
	}
	if !clone.NoSplit || !clone.ManagedFrame {
		t.Error("clone lost the frame flags the budget reads")
	}

	// Mutating the clone must not reach the original.
	clone.Blocks[0].Instrs = nil
	if got := f.String(); got != before {
		t.Errorf("mutating the clone changed the original:\n%s", got)
	}
}

func TestReplaceBodyFromRestoresAMutatedFunction(t *testing.T) {
	m, f := sampleFunc()
	before := f.String()

	snapshot, err := CloneFunc(f)
	if err != nil {
		t.Fatalf("CloneFunc: %v", err)
	}
	f.Blocks[0].Instrs = f.Blocks[0].Instrs[:1]
	if f.String() == before {
		t.Fatal("the mutation did not take")
	}

	f.ReplaceBodyFrom(snapshot)
	if got := f.String(); got != before {
		t.Errorf("restore did not reproduce the original:\n--- original\n%s\n--- restored\n%s", before, got)
	}
	if f.Module() != m {
		t.Error("restore replaced the function's owning module")
	}
	if f.Name != "caller" {
		t.Errorf("restore changed the name to %q", f.Name)
	}
}
