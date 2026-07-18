package difftest

import (
	"testing"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/interp"
	"github.com/evanphx/cg12/obj"
	"github.com/evanphx/cg12/parse"
	"github.com/stretchr/testify/require"
)

// The interpreter lays global data into memory the same bytes the arm64 backend
// emits into the object — the two must agree at the byte level, which is what
// makes the interpreter a faithful oracle for programs that read initialized
// globals. This compares the two images for the same ir.Data without running
// anything (or needing a toolchain).
func TestInterpGlobalImageMatchesArm64(t *testing.T) {
	const il = `data $g = align 8 { l 100, l 200, w 42, h 7, b 255, b 1, s s_1.5, d d_2.5 }
	export function w $anchor() { @start ret 0 }`

	// arm64's data image for symbol $g. CompileToObject lowers, so parse a fresh
	// module for the interpreter below.
	ma, err := parse.Parse(il)
	require.NoError(t, err)
	o, err := arm64.CompileToObject(ma)
	require.NoError(t, err)

	var sym obj.Sym
	found := false
	for _, s := range o.Syms {
		if s.Name == "g" {
			sym, found = s, true
			break
		}
	}
	require.True(t, found, "arm64 object defines g")
	want := sectionBytes(t, o, sym)

	mi, err := parse.Parse(il)
	require.NoError(t, err)
	mc, err := interp.New(mi)
	require.NoError(t, err)
	addr, ok := mc.GlobalAddr("g")
	require.True(t, ok)
	got, err := mc.ReadBytes(addr, len(want))
	require.NoError(t, err)

	require.Equal(t, want, got, "interpreter global image must match the arm64 data emission byte-for-byte")
}

// sectionBytes returns the bytes a symbol occupies in its section of an object.
func sectionBytes(t *testing.T, o *obj.Object, s obj.Sym) []byte {
	t.Helper()
	var sec []byte
	switch s.Section {
	case obj.SecData:
		sec = o.Data
	case obj.SecRodata:
		sec = o.Rodata
	default:
		t.Fatalf("symbol g is in unexpected section %v", s.Section)
	}
	end := s.Value + s.Size
	require.LessOrEqual(t, end, uint64(len(sec)))
	return sec[s.Value:end]
}
