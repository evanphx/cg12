package obj_test

import (
	"testing"

	"github.com/evanphx/cg12/arm64/a64"
	"github.com/evanphx/cg12/obj"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func symByName(syms []obj.Sym) map[string]obj.Sym {
	m := map[string]obj.Sym{}
	for _, s := range syms {
		m[s.Name] = s
	}
	return m
}

func TestReadELFRoundTrip(t *testing.T) {
	text := words(a64.AddReg(false, 0, 0, 1), a64.Bl(0), a64.Ret(30))
	o := &obj.Object{
		Machine: obj.EM_AARCH64,
		Text:    text,
		Data:    []byte{1, 2, 3, 4, 5, 6, 7, 8},
		Syms: []obj.Sym{
			{Name: "f", Section: obj.SecText, Size: uint64(len(text)), Global: true, Func: true},
			{Name: "d", Section: obj.SecData, Value: 0, Size: 8, Global: true},
		},
		Relocs:     []obj.Reloc{{Offset: 4, Sym: "ext", Type: obj.R_AARCH64_CALL26}},
		DataRelocs: []obj.Reloc{{Offset: 0, Sym: "f", Type: obj.R_AARCH64_ABS64, Addend: 4}},
	}
	data, err := o.MarshalELF()
	require.NoError(t, err)

	got, err := obj.ReadELF(data)
	require.NoError(t, err)
	assert.Equal(t, o.Text, got.Text)
	assert.Equal(t, o.Data, got.Data)

	by := symByName(got.Syms)
	require.Contains(t, by, "f")
	assert.Equal(t, obj.SecText, by["f"].Section)
	assert.True(t, by["f"].Func)
	assert.Equal(t, obj.SecData, by["d"].Section)
	assert.Equal(t, obj.SecUndef, by["ext"].Section, "reloc target is undefined")

	require.Len(t, got.Relocs, 1)
	assert.Equal(t, uint64(4), got.Relocs[0].Offset)
	assert.Equal(t, "ext", got.Relocs[0].Sym)
	assert.Equal(t, uint32(obj.R_AARCH64_CALL26), got.Relocs[0].Type)

	require.Len(t, got.DataRelocs, 1)
	assert.Equal(t, "f", got.DataRelocs[0].Sym)
	assert.Equal(t, int64(4), got.DataRelocs[0].Addend)
}
