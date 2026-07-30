package runtimepack

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func samplePack() *Pack {
	return &Pack{
		Manifest: Manifest{
			Version:             Version,
			Target:              "arm64",
			Fingerprint:         "abc123",
			ModuleDataSymbol:    "runtime_firstmoduledata",
			ProgramModuleSymbol: "goc_programmoduledata",
			Defined:             []string{"runtime_mallocgc", "runtime_newobject"},
			DataDigests:         map[string]string{"runtime_divideError": "deadbeef"},
			AssemblyFiles:       []string{"runtime/asm_arm64.s"},
			ProgramSymbols:      []string{"error_Error", "main_main"},
		},
		Object:  []byte{0x7f, 'E', 'L', 'F', 1, 2, 3},
		Sidecar: []byte{0x7f, 'E', 'L', 'F', 9},
	}
}

func TestPackRoundTrips(t *testing.T) {
	original := samplePack()

	encoded, err := original.Marshal()
	require.NoError(t, err)
	decoded, err := Unmarshal(encoded)
	require.NoError(t, err)

	assert.Equal(t, original.Manifest, decoded.Manifest)
	assert.Equal(t, original.Object, decoded.Object)
	assert.Equal(t, original.Sidecar, decoded.Sidecar)
}

// The container exists so the manifest cannot drift from the objects it
// describes. A file that is not one at all has to say so rather than be read as
// an object.
func TestUnmarshalRefusesSomethingElse(t *testing.T) {
	_, err := Unmarshal([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a prebuilt goc runtime")
}

// A pack from an older compiler describes a subtraction this one no longer
// performs the same way, so it is refused rather than mislinked.
func TestUnmarshalRefusesAnotherVersion(t *testing.T) {
	encoded, err := samplePack().Marshal()
	require.NoError(t, err)
	binary.LittleEndian.PutUint32(encoded[len(magic):], Version+1)

	_, err = Unmarshal(encoded)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "rebuild it")
}

func TestUnmarshalRefusesTruncatedMembers(t *testing.T) {
	encoded, err := samplePack().Marshal()
	require.NoError(t, err)

	_, err = Unmarshal(encoded[:len(encoded)-3])

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the index describes")
}

func TestManifestSets(t *testing.T) {
	manifest := samplePack().Manifest

	assert.Equal(t, map[string]bool{"runtime_mallocgc": true, "runtime_newobject": true}, manifest.DefinedSet())
	assert.Equal(t, map[string]bool{"error_Error": true, "main_main": true}, manifest.ProgramSymbolSet())
	assert.Equal(t, map[string]bool{"runtime/asm_arm64.s": true}, manifest.AssemblyFileSet())
}
