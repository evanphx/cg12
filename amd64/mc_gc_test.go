package amd64_test

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"testing"

	"github.com/evanphx/cg12/amd64"
	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// stackMapSection parses the .cg12_stackmaps section of an object, returning the
// safepoint count and the roots of the first safepoint.
func stackMapSection(t *testing.T, code []byte) (count int, roots []struct {
	kind uint8
	val  int32
	typ  uint32
}) {
	t.Helper()
	f, err := elf.NewFile(bytes.NewReader(code))
	require.NoError(t, err)
	sec := f.Section(".cg12_stackmaps")
	require.NotNil(t, sec, "expected a .cg12_stackmaps section")
	b, err := sec.Data()
	require.NoError(t, err)
	require.Equal(t, "SMAP", string(b[0:4]))
	require.Equal(t, uint16(2), binary.LittleEndian.Uint16(b[4:6]))
	count = int(binary.LittleEndian.Uint32(b[8:12]))
	// First safepoint: pc u64, nroots u32, then roots.
	p := 12 + 8
	n := int(binary.LittleEndian.Uint32(b[p : p+4]))
	p += 4
	for i := 0; i < n; i++ {
		roots = append(roots, struct {
			kind uint8
			val  int32
			typ  uint32
		}{
			kind: b[p],
			val:  int32(binary.LittleEndian.Uint32(b[p+4 : p+8])),
			typ:  binary.LittleEndian.Uint32(b[p+8 : p+12]),
		})
		p += 12
	}
	return count, roots
}

// A managed reference live across a call is forced to a frame slot and reported
// as a frame-relative root at the call's safepoint.
func TestObjStackMapRootAcrossCall(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("f", ir.ClsW).Export()
	seed := f.Param("seed", ir.ClsW)
	e := f.Entry()
	p := e.Alloc(8, 8)
	f.MarkGCRefType(p, 2) // a managed reference (type 2)
	e.Store(seed, p)
	e.CallVoid(f.Sym("sink", 0)) // safepoint; p is live across it
	e.Ret(e.Load(ir.ClsW, p))

	code, err := amd64.CompileObject(m)
	require.NoError(t, err)

	count, roots := stackMapSection(t, code)
	require.Equal(t, 1, count)
	require.Len(t, roots, 1)
	require.Equal(t, uint8(1), roots[0].kind) // frame-relative
	require.Less(t, roots[0].val, int32(0))   // below rbp
	require.Equal(t, uint32(2), roots[0].typ) // carries the type descriptor
}

// An explicit OSafepoint (no call) also records the live roots.
func TestObjStackMapExplicitSafepoint(t *testing.T) {
	m := ir.NewModule()
	f := m.NewFunc("f", ir.ClsW).Export()
	seed := f.Param("seed", ir.ClsW)
	e := f.Entry()
	p := e.Alloc(8, 8)
	f.MarkGCRefType(p, 5)
	e.Store(seed, p)
	e.Safepoint() // explicit poll; p is a live root here
	e.Ret(e.Load(ir.ClsW, p))

	code, err := amd64.CompileObject(m)
	require.NoError(t, err)
	count, roots := stackMapSection(t, code)
	require.Equal(t, 1, count)
	require.Len(t, roots, 1)
	require.Equal(t, uint8(1), roots[0].kind)
	require.Equal(t, uint32(5), roots[0].typ)
}
