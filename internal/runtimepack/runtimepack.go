// Package runtimepack is the on-disk form of a prebuilt goc runtime: the
// relocatable object holding one complete Go module, the assembled Plan 9
// sidecar that module's text ends with, and the manifest describing what the
// object defines.
//
// A prebuilt runtime is useless without its manifest. The program side compiles
// only the difference between what it needs and what the runtime already has, so
// it has to know exactly which symbols the object defines; get that set wrong and
// the result is a duplicate-symbol error at best and two copies of a package
// global at worst. Keeping the objects and the manifest in one versioned file is
// what stops the two from drifting apart.
//
// The alternatives were considered and rejected. An `ar` archive is standard and
// cc consumes it directly, but cc pulls members only to resolve an undefined
// symbol -- a member the image needs but nothing has referenced yet is silently
// dropped -- and an archive has nowhere to put the manifest. A non-allocated ELF
// section inside a single merged object is tidier still, but merging the Go
// object with the assembled sidecar into one ET_REL is a code path nothing else
// in the tree uses, sitting on every build. What is left is this: a magic, a
// version, a JSON index, and the members concatenated after it.
package runtimepack

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
)

// magic identifies the container. It is deliberately not an ELF magic: the file
// is not an object, and a tool that mistakes it for one should fail immediately
// rather than half-read it.
const magic = "cg12gorp"

// Version is the container and manifest version. It is checked on read, so a
// pack written by an older compiler is refused rather than mislinked. Bump it
// whenever the manifest's meaning changes.
const Version = 1

// Manifest describes the prebuilt runtime module.
type Manifest struct {
	Version int `json:"version"`

	// Target is the goc target the runtime was compiled for.
	Target string `json:"target"`

	// Fingerprint identifies the compilation the objects came from: the runtime
	// source identity plus the options that change what is emitted. A program
	// built against a pack records nothing, so the fingerprint is what a caller
	// compares when it wants to know whether a cached pack is still current.
	Fingerprint string `json:"fingerprint"`

	// Optimize records whether the runtime module was compiled with -O. Both
	// halves have to agree: they are one image, and half an image optimized is a
	// combination nothing has ever been tested in.
	Optimize bool `json:"optimize"`

	// ModuleDataSymbol is the runtime module's moduledata record.
	ModuleDataSymbol string `json:"moduleDataSymbol"`

	// ProgramModuleSymbol is the symbol the runtime module's moduledata.next
	// names. The program module must define it, or the link fails.
	ProgramModuleSymbol string `json:"programModuleSymbol"`

	// Defined lists every global symbol the pack's objects define, in linker
	// spelling. The program side subtracts exactly this set.
	Defined []string `json:"defined"`

	// DataDigests maps a defined data symbol to a digest of the items it was
	// built from. The program side compares the digest of the datum it would have
	// emitted and refuses to subtract one that differs, so "same name, different
	// content" is a build error rather than a silent mislink.
	DataDigests map[string]string `json:"dataDigests"`

	// AssemblyFiles lists the Plan 9 assembly sources the pack's sidecar was
	// translated from. A program reaching a package the prebuilt module never
	// loaded -- reflect's methodValueCall, a crypto block function -- still needs
	// that package's assembly, so it translates and assembles what is not here
	// into a sidecar of its own.
	AssemblyFiles []string `json:"assemblyFiles"`

	// ProgramSymbols lists the symbols the runtime object deliberately leaves
	// undefined because they are runtime-named but program-built: main's own
	// functions and the interface-method dispatchers, which switch over the itabs
	// the program contains. The program module must define and export them.
	ProgramSymbols []string `json:"programSymbols"`
}

// Pack is a prebuilt runtime: its manifest and the two objects that make it up.
type Pack struct {
	Manifest Manifest

	// Object is the ELF relocatable holding the runtime module's Go code, data,
	// and complete pclntab/moduledata.
	Object []byte

	// Sidecar is the assembled Plan 9 assembly the Go runtime needs. It carries
	// the module's last text, so it also defines the module's text-end symbol.
	Sidecar []byte
}

// index is the JSON header: the manifest plus the lengths of the members that
// follow it, in order.
type index struct {
	Manifest    Manifest `json:"manifest"`
	ObjectSize  int      `json:"objectSize"`
	SidecarSize int      `json:"sidecarSize"`
}

// Marshal encodes the pack.
func (pack *Pack) Marshal() ([]byte, error) {
	header, err := json.Marshal(index{
		Manifest:    pack.Manifest,
		ObjectSize:  len(pack.Object),
		SidecarSize: len(pack.Sidecar),
	})
	if err != nil {
		return nil, err
	}
	encoded := make([]byte, 0, len(magic)+12+len(header)+len(pack.Object)+len(pack.Sidecar))
	encoded = append(encoded, magic...)
	encoded = binary.LittleEndian.AppendUint32(encoded, uint32(Version))
	encoded = binary.LittleEndian.AppendUint64(encoded, uint64(len(header)))
	encoded = append(encoded, header...)
	encoded = append(encoded, pack.Object...)
	encoded = append(encoded, pack.Sidecar...)
	return encoded, nil
}

// Unmarshal decodes a pack, refusing one this compiler does not understand.
func Unmarshal(encoded []byte) (*Pack, error) {
	if len(encoded) < len(magic)+12 || string(encoded[:len(magic)]) != magic {
		return nil, fmt.Errorf("runtimepack: not a prebuilt goc runtime")
	}
	version := binary.LittleEndian.Uint32(encoded[len(magic):])
	if version != Version {
		return nil, fmt.Errorf("runtimepack: version %d, but this goc writes version %d; rebuild it", version, Version)
	}
	headerSize := binary.LittleEndian.Uint64(encoded[len(magic)+4:])
	body := encoded[len(magic)+12:]
	if uint64(len(body)) < headerSize {
		return nil, fmt.Errorf("runtimepack: truncated header")
	}
	var header index
	if err := json.Unmarshal(body[:headerSize], &header); err != nil {
		return nil, fmt.Errorf("runtimepack: %w", err)
	}
	if header.Manifest.Version != Version {
		return nil, fmt.Errorf("runtimepack: manifest version %d, but this goc writes version %d; rebuild it", header.Manifest.Version, Version)
	}
	members := body[headerSize:]
	if len(members) != header.ObjectSize+header.SidecarSize {
		return nil, fmt.Errorf("runtimepack: %d bytes of members, but the index describes %d",
			len(members), header.ObjectSize+header.SidecarSize)
	}
	return &Pack{
		Manifest: header.Manifest,
		Object:   members[:header.ObjectSize],
		Sidecar:  members[header.ObjectSize:],
	}, nil
}

// Write encodes the pack to a file.
func (pack *Pack) Write(path string) error {
	encoded, err := pack.Marshal()
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o644)
}

// Read decodes a pack from a file.
func Read(path string) (*Pack, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Unmarshal(encoded)
}

// DefinedSet is the manifest's Defined list as a set, which is the form the
// program-side subtraction wants.
func (manifest *Manifest) DefinedSet() map[string]bool {
	defined := make(map[string]bool, len(manifest.Defined))
	for _, symbol := range manifest.Defined {
		defined[symbol] = true
	}
	return defined
}

// AssemblyFileSet is the manifest's AssemblyFiles list as a set.
func (manifest *Manifest) AssemblyFileSet() map[string]bool {
	files := make(map[string]bool, len(manifest.AssemblyFiles))
	for _, path := range manifest.AssemblyFiles {
		files[path] = true
	}
	return files
}

// ProgramSymbolSet is the manifest's ProgramSymbols list as a set.
func (manifest *Manifest) ProgramSymbolSet() map[string]bool {
	symbols := make(map[string]bool, len(manifest.ProgramSymbols))
	for _, symbol := range manifest.ProgramSymbols {
		symbols[symbol] = true
	}
	return symbols
}
