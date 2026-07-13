package amd64_test

import (
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// Reading the stack pointer via a register variable yields the real rsp, near a
// stack address.
func TestObjRegVarReadSP(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("runtest", ir.ClsW).Export()
	e := f.Entry()
	sp := f.RegVar("sp", int(amd64.RSP))
	s0 := e.Load(ir.ClsL, sp)
	// rsp is a high, aligned address; return its low nibble (must be 0 after our
	// 16-aligned frame) so success == 0.
	e.Ret(e.Extuw(ir.ClsW, e.And(ir.ClsL, s0, f.Long(0xF))))
	require.Equal(t, 0, runObj(t, m))
}

// The stack pointer can be written too — the primitive a stack switch needs. This
// reserves scratch below rsp, uses it, and restores rsp.
func TestObjRegVarWriteSP(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("runtest", ir.ClsW).Export()
	e := f.Entry()
	sp := f.RegVar("sp", int(amd64.RSP))
	s0 := e.Load(ir.ClsL, sp)                 // s0 = rsp
	lowered := e.Sub(ir.ClsL, s0, f.Long(64)) // 64 bytes below rsp
	e.Store(lowered, sp)                      // rsp = s0 - 64 (reserve)
	e.StoreSub(ir.SubW, f.Word(0xBEEF), lowered)
	v := e.Load(ir.ClsW, lowered)
	e.Store(s0, sp)                          // restore rsp
	e.Ret(e.Sub(ir.ClsW, v, f.Word(0xBEEF))) // 0 on success
	require.Equal(t, 0, runObj(t, m))
}
