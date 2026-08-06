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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// magic identifies the container. It is deliberately not an ELF magic: the file
// is not an object, and a tool that mistakes it for one should fail immediately
// rather than half-read it.
const magic = "cg12gorp"

// Version is the container and manifest version. It is checked on read, so a
// pack written by an older compiler is refused rather than mislinked. Bump it
// whenever the manifest's meaning changes.
//
// Version 2 added Packages and Closure, which is what lets a build hold several
// packs and give each program the richest one it can use.
//
// Version 3 added the IR member: the optimized prebuilt module, serialized, so a
// program build can inline across the pack boundary instead of compiling against
// a wall of external symbols. It is a container change (a third member) and a
// manifest change (IRVersion, IRDigest), so both checks below move together.
const Version = 3

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

	// Packages are the standard library packages the pack's root asked for,
	// beyond the Go runtime itself. Empty means the runtime alone. It is what a
	// human reads and what a cache key is built from; Closure is what an
	// applicability test uses.
	Packages []string `json:"packages,omitempty"`

	// Closure is every package path the pack's compilation loaded, sorted. A
	// program may use this pack only if its own closure contains all of it: the
	// pack leaves its type region, its dispatchers and its degraded itabs for the
	// program module, and a program that never loaded one of these packages
	// cannot generate them.
	Closure []string `json:"closure,omitempty"`

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

	// IRVersion is the ir binary unit format version the IR member was written
	// with, or 0 when the pack carries none.
	//
	// It is the one clause a pack key needs that an object-only pack did not.
	// Everything else the key already covers -- the target, -O, the placement
	// policy, the pipeline identity, every GOC_/CG12_ variable, the hashed
	// compiler, the hashed stdlib, the C toolchain -- describes what was compiled
	// and how. This describes how the artifact was *written*, which the compiler's
	// own hash does not: ir/binary.go's version byte is data, not code reachable
	// from a hash of the binary in any way a reader can rely on. A pack read back
	// by a compiler whose format has moved is refused here rather than decoded
	// into a module that is subtly not what was written.
	IRVersion int `json:"irVersion,omitempty"`

	// IRDigest is a sha256 of the IR member, checked on read.
	//
	// gc has the same thing for the same reason: cmd/link's checkFingerprint
	// refuses an import whose fingerprint does not match, so a stale or truncated
	// artifact is a loud failure rather than a miscompile. ir/binary.go has a
	// magic tag and a version byte and no content digest, so the check lives here.
	IRDigest string `json:"irDigest,omitempty"`
}

// DigestOf is the content digest a pack records for a member. Empty for an empty
// member, so a pack carrying no IR records no digest rather than the digest of
// nothing.
func DigestOf(member []byte) string {
	if len(member) == 0 {
		return ""
	}
	sum := sha256.Sum256(member)
	return hex.EncodeToString(sum[:])
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

	// IR is the optimized prebuilt module in ir/binary.go's unit format, or empty
	// for an object-only pack.
	//
	// It is what the program side inlines from. The object above is still the
	// pack's own code -- carrying IR does not mean giving up the compiled member,
	// and gc does not either: its archive holds both the compiled package and the
	// export data whose bodies importers inline.
	IR []byte
}

// index is the JSON header: the manifest plus the lengths of the members that
// follow it, in order.
type index struct {
	Manifest    Manifest `json:"manifest"`
	ObjectSize  int      `json:"objectSize"`
	SidecarSize int      `json:"sidecarSize"`
	IRSize      int      `json:"irSize"`
}

// Marshal encodes the pack.
func (pack *Pack) Marshal() ([]byte, error) {
	header, err := json.Marshal(index{
		Manifest:    pack.Manifest,
		ObjectSize:  len(pack.Object),
		SidecarSize: len(pack.Sidecar),
		IRSize:      len(pack.IR),
	})
	if err != nil {
		return nil, err
	}
	encoded := make([]byte, 0, len(magic)+12+len(header)+len(pack.Object)+len(pack.Sidecar)+len(pack.IR))
	encoded = append(encoded, magic...)
	encoded = binary.LittleEndian.AppendUint32(encoded, uint32(Version))
	encoded = binary.LittleEndian.AppendUint64(encoded, uint64(len(header)))
	encoded = append(encoded, header...)
	encoded = append(encoded, pack.Object...)
	encoded = append(encoded, pack.Sidecar...)
	encoded = append(encoded, pack.IR...)
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
	described := header.ObjectSize + header.SidecarSize + header.IRSize
	if len(members) != described {
		return nil, fmt.Errorf("runtimepack: %d bytes of members, but the index describes %d",
			len(members), described)
	}
	pack := &Pack{
		Manifest: header.Manifest,
		Object:   members[:header.ObjectSize],
		Sidecar:  members[header.ObjectSize : header.ObjectSize+header.SidecarSize],
		IR:       members[header.ObjectSize+header.SidecarSize:],
	}
	// Checked here rather than where the IR is decoded, so that every reader gets
	// the check and no caller can forget it. A pack whose IR does not hash to what
	// its manifest recorded is a corrupt or truncated artifact, and decoding it
	// would produce a module that is not the one the pack was built from.
	if digest := DigestOf(pack.IR); digest != header.Manifest.IRDigest {
		return nil, fmt.Errorf("runtimepack: the IR member hashes to %.16s, but the manifest records %.16s",
			orNone(digest), orNone(header.Manifest.IRDigest))
	}
	return pack, nil
}

func orNone(digest string) string {
	if digest == "" {
		return "no IR"
	}
	return digest
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

// ReadManifest decodes only a pack's manifest, without its members.
//
// Choosing between several packs needs each one's closure and nothing else, and
// a pack carrying the standard library is tens of megabytes. Reading the header
// alone keeps the choice proportional to the number of candidates rather than to
// their size.
func ReadManifest(path string) (*Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	prologue := make([]byte, len(magic)+12)
	if _, err := io.ReadFull(file, prologue); err != nil {
		return nil, fmt.Errorf("runtimepack: not a prebuilt goc runtime: %s", path)
	}
	if string(prologue[:len(magic)]) != magic {
		return nil, fmt.Errorf("runtimepack: not a prebuilt goc runtime: %s", path)
	}
	version := binary.LittleEndian.Uint32(prologue[len(magic):])
	if version != Version {
		return nil, fmt.Errorf("runtimepack: %s is version %d, but this goc writes version %d; rebuild it", path, version, Version)
	}
	headerSize := binary.LittleEndian.Uint64(prologue[len(magic)+4:])
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(file, header); err != nil {
		return nil, fmt.Errorf("runtimepack: truncated header in %s", path)
	}
	var decoded index
	if err := json.Unmarshal(header, &decoded); err != nil {
		return nil, fmt.Errorf("runtimepack: %w", err)
	}
	if decoded.Manifest.Version != Version {
		return nil, fmt.Errorf("runtimepack: %s has manifest version %d, but this goc writes version %d; rebuild it",
			path, decoded.Manifest.Version, Version)
	}
	return &decoded.Manifest, nil
}

// UsableBy reports whether a program whose loaded package closure is closure may
// be compiled against this pack.
//
// The condition is containment, and it is the whole of the safety argument for
// carrying the standard library in a pack. The pack leaves its Go type region,
// its interface dispatchers and its degraded itabs for the program module to
// define -- and a program only generates those for packages it loaded. A program
// that loaded everything the pack did generates a superset of them, so the
// subtraction still closes; a program that loaded less does not, and the build
// has to fall back to a smaller pack rather than produce an image missing type
// descriptors.
func (manifest *Manifest) UsableBy(closure map[string]bool) bool {
	for _, path := range manifest.Closure {
		if !closure[path] {
			return false
		}
	}
	return true
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
