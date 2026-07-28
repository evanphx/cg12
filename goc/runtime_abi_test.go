package goc

import (
	"go/token"
	"go/types"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeAssemblyOffsetsMatchStandardLibraryTypes(t *testing.T) {
	loader := newSourceLoader(token.NewFileSet(), HostTarget())
	pkg, err := loader.Import("runtime")
	require.NoError(t, err)

	expected := []struct {
		typeName string
		fields   map[string]int64
		size     int64
	}{
		{
			typeName: "stack",
			fields: map[string]int64{
				"lo": 0,
				"hi": 8,
			},
			size: 16,
		},
		{
			typeName: "gobuf",
			fields: map[string]int64{
				"sp":   0,
				"pc":   8,
				"g":    16,
				"ctxt": 24,
				"lr":   32,
				"bp":   40,
			},
			size: 48,
		},
		{
			typeName: "g",
			fields: map[string]int64{
				"stack":       0,
				"stackguard0": 16,
				"stackguard1": 24,
				"m":           48,
				"sched":       56,
			},
		},
		{
			typeName: "m",
			fields: map[string]int64{
				"g0":      0,
				"morebuf": 8,
				"curg":    184,
				"p":       208,
			},
		},
		{
			typeName: "moduledata",
			fields: map[string]int64{
				"pcHeader":  0,
				"ftab":      128,
				"minpc":     160,
				"maxpc":     168,
				"text":      176,
				"etext":     184,
				"inittasks": 472,
				"next":      584,
			},
			size: 592,
		},
	}

	sizes := types.SizesFor("gc", "arm64")
	for _, test := range expected {
		t.Run(test.typeName, func(t *testing.T) {
			object := pkg.Scope().Lookup(test.typeName)
			require.NotNil(t, object)
			structure, ok := object.Type().Underlying().(*types.Struct)
			require.True(t, ok)
			offsets := sizes.Offsetsof(structFields(structure))
			for fieldName, want := range test.fields {
				index := runtimeFieldIndex(structure, fieldName)
				require.NotEqual(t, -1, index, "missing runtime.%s.%s", test.typeName, fieldName)
				assert.Equal(t, want, offsets[index], "runtime.%s.%s", test.typeName, fieldName)
			}
			if test.size != 0 {
				assert.Equal(t, test.size, sizes.Sizeof(structure), "sizeof runtime.%s", test.typeName)
			}
		})
	}
}

func TestInternalABIArrayTypeOffsetsMatchRuntime(t *testing.T) {
	loader := newSourceLoader(token.NewFileSet(), HostTarget())
	pkg, err := loader.Import("internal/abi")
	require.NoError(t, err)

	object := pkg.Scope().Lookup("ArrayType")
	require.NotNil(t, object)
	structure, ok := object.Type().Underlying().(*types.Struct)
	require.True(t, ok)

	sizes := types.SizesFor("gc", "arm64")
	offsets := sizes.Offsetsof(structFields(structure))
	assert.Equal(t, int64(48), offsets[runtimeFieldIndex(structure, "Elem")])
	assert.Equal(t, int64(56), offsets[runtimeFieldIndex(structure, "Slice")])
	assert.Equal(t, int64(64), offsets[runtimeFieldIndex(structure, "Len")])
	assert.Equal(t, int64(72), sizes.Sizeof(structure))
}

func runtimeFieldIndex(structure *types.Struct, name string) int {
	for index := 0; index < structure.NumFields(); index++ {
		if structure.Field(index).Name() == name {
			return index
		}
	}
	return -1
}
