package amd64_test

import (
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/require"
)

// Reading a thread-local variable uses the local-exec model: a TP-relative offset
// relocation against an STT_TLS symbol. (Freestanding qemu has no thread-pointer
// setup, so this is checked at the object level rather than run.)
func TestObjTLSLocalExec(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("readtls", ir.ClsL).Export()
	e := f.Entry()
	addr := f.ThreadSym("tlsvar")
	e.Ret(e.Load(ir.ClsL, addr))

	code, err := amd64.CompileObject(m)
	require.NoError(t, err)

	o, err := obj.ReadELF(code)
	require.NoError(t, err)

	var found bool
	for _, r := range o.Relocs {
		if r.Sym == "tlsvar" {
			require.Equal(t, uint32(obj.R_X86_64_TPOFF32), r.Type)
			found = true
		}
	}
	require.True(t, found, "expected a TPOFF32 relocation against tlsvar")

	// The referenced symbol must be typed STT_TLS.
	var tlsTyped bool
	for _, s := range o.Syms {
		if s.Name == "tlsvar" {
			tlsTyped = s.TLS
		}
	}
	require.True(t, tlsTyped, "tlsvar must be an STT_TLS symbol")
}
