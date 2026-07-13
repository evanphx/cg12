package ir

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// buildPointerFunc builds a function that returns a pointer and exercises a
// pointer load, a pointer store, and — for contrast — a genuine 64-bit long
// load that must not be narrowed.
func buildPointerFunc() *Func {
	f := NewModule().NewFunc("p", ClsP)
	a := f.Param("a", ClsP)
	e := f.Entry()
	pv := e.Load(ClsP, a) // pointer load  -> OLoadl / ClsP
	e.Store(pv, a)        // pointer store -> OStorel, value ClsP
	_ = e.Load(ClsL, a)   // genuine long load -> OLoadl / ClsL (unchanged)
	e.Ret(pv)
	return f
}

func TestGCRefSurvivesLowering(t *testing.T) {
	f := NewModule().NewFunc("g", ClsL)
	r := f.ParamRef("obj") // a managed reference: ClsP + GCRef
	assert.True(t, f.Temp(r).GCRef)
	assert.Equal(t, ClsP, f.Temp(r).Cls)

	// Pointer lowering narrows the class but must not clear the GC-root flag.
	LowerPointers(f, ClsL)
	assert.True(t, f.Temp(r).GCRef, "GCRef survives lowering")
	assert.Equal(t, ClsL, f.Temp(r).Cls)
}

func TestLowerPointersToWord(t *testing.T) {
	f := buildPointerFunc()
	LowerPointers(f, ClsW)

	e := f.Start
	// The pointer parameter and result temporary are now words.
	assert.Equal(t, ClsW, f.Params[0].Cls)
	assert.Equal(t, ClsW, f.Retty)
	assert.Equal(t, ClsW, f.ClassOf(e.Jmp.Arg))

	// Pointer load/store narrowed to the 4-byte memory image.
	assert.Equal(t, OLoaduw, e.Instrs[0].Op, "pointer load narrowed")
	assert.Equal(t, ClsW, e.Instrs[0].Cls)
	assert.Equal(t, OStorew, e.Instrs[1].Op, "pointer store narrowed")
	// The genuine long load is untouched.
	assert.Equal(t, OLoadl, e.Instrs[2].Op)
	assert.Equal(t, ClsL, e.Instrs[2].Cls)

	// No ClsP survives the pass.
	for _, tmp := range f.Temps {
		assert.NotEqual(t, ClsP, tmp.Cls)
	}
}

func TestLowerPointersToLong(t *testing.T) {
	f := buildPointerFunc()
	LowerPointers(f, ClsL)

	e := f.Start
	assert.Equal(t, ClsL, f.Params[0].Cls)
	assert.Equal(t, ClsL, f.Retty)
	// On a 64-bit target pointer memory ops stay full-width.
	assert.Equal(t, OLoadl, e.Instrs[0].Op)
	assert.Equal(t, OStorel, e.Instrs[1].Op)
	assert.Equal(t, OLoadl, e.Instrs[2].Op)
}

func TestPointerClassProperties(t *testing.T) {
	assert.True(t, ClsP.IsInt())
	assert.True(t, ClsP.IsPtr())
	assert.False(t, ClsP.IsFloat())
	assert.Equal(t, "p", ClsP.String())
}
