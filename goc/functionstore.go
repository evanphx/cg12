package goc

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/evanphx/cg12/internal/cachefile"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
)

// This file is the on-disk half of the function-granular cache: what a cached
// declaration is, how a package's worth of them is written and read back, and
// what has to be replayed alongside the IR for the rest of the compile to be the
// compile it would have been.
//
// Two granularities, on purpose.
//
//   - The unit of *validation* is one function, because that is the unit the
//     property in goc/functionlowering_test.go is about, and because
//     FunctionCacheEntry.Valid has to be able to name the clause that moved.
//   - The unit of *storage* is one package, because a module is roughly 5100
//     functions and a file per function is 5100 tiny reads on every warm compile.
//     One file per package is 50-250 reads, which is the same order as gc's one
//     archive per package, and one read per package is what the operating system
//     is good at.
//
// The cost of that choice is stated rather than hidden: a single changed function
// rewrites its whole package's file, which for `runtime` is a few megabytes. That
// is the right trade for a compile-time cache -- the write happens on the compile
// that was slow anyway, and the read it buys back happens on every compile after.
//
// What a cached declaration holds
//
// Not "a function". A declaration's lowering appends to five things at once:
// mod.Funcs (the function, plus any closure, defer wrapper or method value it
// creates), mod.Data, mod.Types, mod.Files, and seven content-keyed intern tables
// on the generator. Replaying only the functions would leave the next declaration
// that wants the same string literal minting a second copy of a symbol that is
// already in the module. So the unit is the whole delta: everything the compile
// gained while that one declaration was being lowered.
//
// What it deliberately does NOT hold is anything the optimiser produced.
// goc/functionoptimiser_test.go measures why: of 2453 cacheable functions two
// programs share, 518 do not survive opt.OptimizeModule in both, because
// DeadFunc's answer is a whole-program fact. The cache stores pre-optimiser IR
// and the warm compile re-optimises the assembled module in full.

// ---------------------------------------------------------------------------
// The intern journal
// ---------------------------------------------------------------------------

// internKind names one of the generator's content-keyed tables.
type internKind uint8

const (
	// internLiteralData is gen.literalData: byte-valued data symbols by contents.
	internLiteralData internKind = iota
	// internContentSymbol is gen.contentSymbols.
	internContentSymbol
	// internFunctionDescriptor is gen.functionDescriptors.
	internFunctionDescriptor
	// internTypeTag is gen.typeTags.
	internTypeTag
	// internRuntimeType is gen.runtimeTypes, recorded as the *pointer* type's key
	// rather than as a types.Type, because populateRuntimePointerTypes is its only
	// reader and that is the only thing it asks for.
	internRuntimeType
	// internInterfaceItab is gen.interfaceItabs.
	internInterfaceItab
	// internInterfaceCallWrapper is gen.interfaceCallWrappers.
	internInterfaceCallWrapper
	// internGoABIType is gen.goABITypes. The value is the aggregate's name, which
	// is content-derived, so the replay finds the decoded *ir.AggType by it.
	internGoABIType
	// internTypeEqualTarget is not an intern table at all: it is the one write
	// lowering makes to a datum an EARLIER declaration emitted.
	// ensureRuntimeTypeEqual generates a type's equality routine and then points
	// that type's descriptor at it, and the descriptor generally belongs to
	// whichever declaration first needed the type. A delta that records only what
	// its declaration ADDED cannot express that, so the write is journalled and
	// replayed: key is the descriptor's symbol, value is the function descriptor
	// to point it at.
	//
	// It is the reason this list is a journal of operations rather than a set of
	// map deltas. Finding it took a whole-program comparison of an http build --
	// one datum in 42130, `crypto/x509.UnknownAuthorityError`'s descriptor, whose
	// Equal field was a function pointer cold and nil warm.
	internTypeEqualTarget
)

// internNote is one entry added to one of those tables.
type internNote struct {
	Kind  internKind
	Key   string
	Value string
}

// ---------------------------------------------------------------------------
// The artifact journal
// ---------------------------------------------------------------------------

// An interned artifact is a definition that belongs to no declaration. The first
// declaration to want `.goc.type.time.Time.<sha8>` mints it; every later one gets
// a table hit and appends nothing. Recording only what a declaration APPENDED
// therefore records the reference and not the definition, and a unit written that
// way is usable only by a program that happens to contain whichever declaration
// minted it first. Across programs it is not: 357 of 408 corpus programs failed
// to link against a cache one program had filled.
//
// So a unit has to carry the definitions of the artifacts it references. It also
// has to carry the POSITION of each reference, and that is the part that is easy
// to miss. A cold compile mints an artifact at the point of first reference, in
// the middle of some declaration's delta -- and Module.Data order is the order
// arm64/assembly.go emits data in, so it is the image's layout. Carrying the
// definitions but not their positions gives a program that links and is still a
// different binary: measured, filling from hello.go and compiling fmt_sprintf.go
// against it produced exactly the same symbols with 1874 of them at different
// addresses.
//
// The journal therefore records, alongside the table entries, the extent of every
// artifact minted and the position of every artifact merely referenced. From that
// a declaration's stored delta is the sequence its lowering WOULD have produced
// had the intern tables been empty when it started: its own items, and at the
// exact point of first reference the whole definition of each artifact it
// touched. Replay walks that sequence and appends only what the module does not
// already define -- which is what a cold compile does too, so the warm module
// comes out index for index the same.

type artifactEventKind uint8

const (
	// artifactBegin and artifactEnd bracket one artifact's definition. Everything
	// appended between them belongs to that artifact rather than to the
	// declaration that happened to reach it first.
	artifactBegin artifactEventKind = iota
	artifactEnd
	// artifactReference is a table hit: the artifact was already interned, so
	// nothing was appended, and all that is recorded is where the reference fell.
	artifactReference
)

// artifactEvent is one such mark, with the module's three tables, the note list
// and the whole-program counter measured at the moment it was made.
type artifactEvent struct {
	Kind      artifactEventKind
	Symbol    string
	Funcs     int
	Data      int
	Types     int
	Notes     int
	Dependent int
}

// internJournal records the table entries in insertion order and the artifact
// events interleaved with them. A nil journal is the off state and costs one
// pointer comparison at each site that writes to it.
type internJournal struct {
	notes  []internNote
	events []artifactEvent
	// files are the module file indices lowering resolved a source position
	// against, in the order it first reached each. See [internJournal.file] for
	// why the list is of touches rather than of appends.
	files []uint32
	// filesFloor is where the declaration being lowered starts in that list.
	// Deduplication stops there: see [internJournal.beginFileScope].
	filesFloor int
	// dependent counts the times lowering consulted a whole-program fact. See
	// [gen.wholeProgramLowering].
	dependent int
}

// file records that lowering resolved a position in one source file.
//
// Every file, not only the ones this declaration was the first in the PROGRAM to
// reach. That distinction is the whole reason this exists. Module.Files is
// append-on-first-use, so "the files a declaration added" is a fact about which
// declarations ran before it, and a delta that stored it carried one program's
// ordering into another: a declaration that added [a, c] in the program that
// filled the cache adds [a, b, c] in a program that had not seen b, and replaying
// the stored answer puts b at the end instead of the middle. The module still
// contains every file either way -- remapFilePositions appends whatever the unit
// references -- so nothing dangles; the file TABLE comes out in a different
// order, and that is DWARF's file numbering.
//
// The files a declaration touched, in first-touch order, is a fact about the
// declaration. Replaying it through Module.File appends exactly the ones the
// receiving module lacks, in that order, which is what the cold compile did.
//
// The deduplication is per declaration and never reaches past
// [internJournal.filesFloor], because a list that collapsed a declaration's first
// touch into the previous declaration's last one would be program-dependent in
// exactly the way this is here to prevent -- and was, on the first attempt: 37
// declarations recorded an empty file list in one program and a one-file list in
// the other, according to whether the declaration lowered before them happened to
// be in the same file.
//
// The scan is backwards from the end because g.at is called for essentially every
// AST node and consecutive nodes are almost always in one file, so the first
// comparison answers nearly every call. What it walks otherwise is the distinct
// files of one declaration, which is one plus whatever it inlined.
func (j *internJournal) file(index uint32) {
	if j == nil {
		return
	}
	for position := len(j.files) - 1; position >= j.filesFloor; position-- {
		if j.files[position] == index {
			return
		}
	}
	j.files = append(j.files, index)
}

// beginFileScope says that a new declaration's lowering starts here, so that its
// first file touch is recorded even when the declaration before it ended in the
// same file.
func (j *internJournal) beginFileScope() {
	if j == nil {
		return
	}
	j.filesFloor = len(j.files)
}

// touchedFileNames is the distinct files a declaration reached, in first-touch
// order, named rather than numbered so that the receiving module can resolve them
// against its own table.
func touchedFileNames(module *ir.Module, touches []uint32) []string {
	if len(touches) == 0 {
		return nil
	}
	seen := make(map[uint32]bool, len(touches))
	names := make([]string, 0, len(touches))
	for _, index := range touches {
		if index == 0 || seen[index] {
			continue
		}
		seen[index] = true
		names = append(names, module.FileName(index))
	}
	return names
}

func (j *internJournal) note(kind internKind, key, value string) {
	if j == nil {
		return
	}
	j.notes = append(j.notes, internNote{Kind: kind, Key: key, Value: value})
}

func (j *internJournal) event(kind artifactEventKind, symbol string, module *ir.Module) {
	if j == nil {
		return
	}
	j.events = append(j.events, artifactEvent{
		Kind:      kind,
		Symbol:    symbol,
		Funcs:     len(module.Funcs),
		Data:      len(module.Data),
		Types:     len(module.Types),
		Notes:     len(j.notes),
		Dependent: j.dependent,
	})
}

// here is where the module and the journal stand now, in the shape the scope
// reconstruction wants for the end of a declaration.
func (j *internJournal) here(module *ir.Module) artifactEvent {
	event := artifactEvent{Funcs: len(module.Funcs), Data: len(module.Data), Types: len(module.Types)}
	if j != nil {
		event.Notes = len(j.notes)
		event.Dependent = j.dependent
	}
	return event
}

func (j *internJournal) mark(module *ir.Module) declarationMark {
	mark := declarationMark{
		funcs: len(module.Funcs),
		data:  len(module.Data),
		types: len(module.Types),
	}
	if j != nil {
		mark.notes = len(j.notes)
		mark.events = len(j.events)
		mark.files = len(j.files)
		mark.dependent = j.dependent
	}
	return mark
}

// wholeProgramLowering records that what is being lowered right now consulted a
// fact about the whole program rather than about its own package.
//
// There is one such site that runs by default, and it is not the devirtualisation
// gocLoweringUsesWholeProgramFacts turns off. materialiseInterfaceImplementations
// walks every type in the PROGRAM that implements the interface being converted
// to, and mints each one's descriptor and itab -- deliberately, because
// runtime.getitab walks those method tables and dropping them made a reflective
// assertion answer no. The IR of the function is unaffected, which is why the
// side effect was kept; Module.Data is not.
//
// That was invisible while a unit recorded only references to interned artifacts:
// whatever set of itabs the program materialised, the program materialised. Now
// that a unit carries the definitions it references, a declaration whose delta
// includes the materialisation would carry ONE program's implementation set into
// another, and the corpus says so -- 89 of 406 programs came out with the itabs of
// whatever filled the cache rather than their own. So such a declaration is not a
// unit, and lowering says so here rather than the cache guessing from the shape of
// what came out.
func (g *gen) wholeProgramLowering() {
	if g.interns != nil {
		g.interns.dependent++
	}
}

// mintArtifact opens the recording scope of one interned artifact and returns the
// call that closes it, for `defer g.mintArtifact(symbol)()` at each of the sites
// that mint one. Everything appended between the two belongs to the artifact
// rather than to whichever declaration happened to reach it first.
func (g *gen) mintArtifact(symbol string) func() {
	g.interns.event(artifactBegin, symbol, g.mod)
	return func() { g.interns.event(artifactEnd, symbol, g.mod) }
}

// useArtifact records that lowering reached an artifact that was already
// interned. Nothing was appended, so the position is the whole record: a cold
// compile of a program that did not already have it would have minted the
// definition right here, and that is where a replay has to put it.
func (g *gen) useArtifact(symbol string) {
	g.interns.event(artifactReference, symbol, g.mod)
}

// ---------------------------------------------------------------------------
// The stored form
// ---------------------------------------------------------------------------

// packageUnitVersion is the version of the *file layout* below, distinct from
// [FunctionCacheUnitVersion], which versions the meaning of a key. Either one
// moving is a miss; they move for different reasons and a reader should be able
// to tell which.
const packageUnitVersion = 3

const packageUnitMagic = "gocfn1"

// artifactReferenceRecord is one reference to an interned artifact, placed within
// the sequence of items its referrer appended itself.
//
// The three counts are how many of the referrer's OWN functions, data and
// aggregate types precede the reference. They are the positions the merge splices
// the artifact's definition in at, and they are what makes a replayed module
// index for index a cold one rather than merely equivalent to it.
type artifactReferenceRecord struct {
	Symbol string
	Funcs  uint32
	Data   uint32
	Types  uint32
}

// cachedSequence is what one declaration or one interned artifact contributed to
// a module: the items it appended itself, the artifacts it referenced and where,
// and the intern-table entries it made directly.
//
// The same shape serves both because an artifact's definition is a delta in
// exactly the sense a declaration's is -- minting an itab mints the type
// descriptors and the call wrappers it names -- so an artifact's own references
// nest, and the merge resolves them by the same walk.
type cachedSequence struct {
	// Unit is an ir.Module encoding of the own items: the functions, the data and
	// the aggregate types, with a file table private to the unit. SrcPos.File
	// inside it indexes that private table and means nothing until the merge
	// remaps it.
	Unit []byte
	// Refs are the artifact references, in the order they were made.
	Refs []artifactReferenceRecord
	// Interns are the intern-table entries made directly here -- not the ones made
	// inside a referenced artifact, which travel with that artifact so that
	// skipping a definition the module already has skips its table entry too.
	Interns []internNote
}

// cachedArtifact is one interned artifact's definition, stored once per package
// file however many of the file's declarations name it.
type cachedArtifact struct {
	// Symbol is the artifact's head symbol -- the name a referrer names it by, and
	// the name whose presence in the module means the whole definition is present.
	Symbol string
	cachedSequence
}

// cachedDeclaration is one source declaration's whole lowering delta.
type cachedDeclaration struct {
	// Decl identifies the declaration within its package, as trimmed file path,
	// line and column.
	//
	// Not the lowered symbol, which is what this obviously wanted to be. A
	// functionDecl carries a symbol only when the front end had to compute one --
	// for an instantiation -- and for the other 97% of declarations funcDecl
	// derives the name from the type object as it lowers. A warm compile has to
	// decide the hit BEFORE it lowers, so it cannot ask for a name that lowering
	// produces, and reimplementing functionSymbol here would be a second spelling
	// of the naming rules that is correct until someone changes the first.
	//
	// The source position is what the compile does have, is unique within a
	// package by construction, and moves exactly when the package's source moves --
	// which is already a clause of the key, so it cannot go stale on its own.
	Decl string
	// Symbol is the lowered symbol of the first function in the delta. Nothing
	// keys on it; it is here so that a diagnostic about a cached unit can name a
	// function rather than a file and a line.
	Symbol string
	// Files are the source files the declaration's lowering resolved a position
	// in, in first-touch order. Replaying them in order through Module.File --
	// which appends only what is not already there -- is what keeps the warm
	// module's file table identical to the cold one's rather than merely
	// equivalent. See [internJournal.file] for why it is the touches and not the
	// appends.
	Files []string
	cachedSequence
}

// packageCacheUnit is one package's file: its key, and every cacheable
// declaration of it some compile has lowered.
type packageCacheUnit struct {
	// Entry is the key every declaration in the file was written under. It is one
	// per file rather than one per declaration because every clause of it --
	// package source, import identities, target, -O, layout, pipeline, compiler --
	// is a property of the package and the compile, not of the function. Storing
	// it once and validating it per declaration is what "validate per function,
	// store per package" means in practice.
	Entry *FunctionCacheEntry
	// Decls are the declarations, keyed by [cachedDeclaration.Decl].
	Decls map[string]*cachedDeclaration
	// Artifacts are the interned artifacts those declarations reference,
	// transitively, keyed by head symbol. One copy per file however many
	// declarations of the file name it: the duplication this design pays is across
	// package files, not across the declarations inside one.
	Artifacts map[string]*cachedArtifact
	// order is the declaration keys in write order, so the file is
	// byte-deterministic.
	order []string
}

func newPackageCacheUnit(entry *FunctionCacheEntry) *packageCacheUnit {
	return &packageCacheUnit{
		Entry:     entry,
		Decls:     map[string]*cachedDeclaration{},
		Artifacts: map[string]*cachedArtifact{},
	}
}

func (u *packageCacheUnit) add(declaration *cachedDeclaration) {
	if _, exists := u.Decls[declaration.Decl]; !exists {
		u.order = append(u.order, declaration.Decl)
	}
	u.Decls[declaration.Decl] = declaration
}

// artifactNames is the file's artifacts in a deterministic order.
func (u *packageCacheUnit) artifactNames() []string {
	names := make([]string, 0, len(u.Artifacts))
	for symbol := range u.Artifacts {
		names = append(names, symbol)
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// The key, and where a package's file lives
// ---------------------------------------------------------------------------

// packageCacheKeyDigest folds every clause of an entry into the name its file is
// stored under.
//
// It is a content address of the key, which means a compile does not have to open
// a file to find out whether it is looking at the right one: if the digest
// differs the path differs. FunctionCacheEntry.Valid still runs on what comes
// back, because a content address answers "is this the file for this key" and
// not "is this file the file it says it is".
//
// The clauses are written with lengths and separators rather than concatenated,
// so that no two different keys can spell the same byte string.
func packageCacheKeyDigest(entry *FunctionCacheEntry) string {
	digest := sha256.New()
	fmt.Fprintf(digest, "gocfn\x00%d\x00%d\x00", packageUnitVersion, entry.UnitVersion)
	fmt.Fprintf(digest, "package\x00%d\x00%s\x00", len(entry.Package), entry.Package)
	fmt.Fprintf(digest, "source\x00%x\x00", entry.Source)
	fmt.Fprintf(digest, "target\x00%s\x00optimize\x00%v\x00", entry.Target, entry.Optimize)
	fmt.Fprintf(digest, "layout\x00%d\x00%s\x00", len(entry.TextLayout), entry.TextLayout)
	fmt.Fprintf(digest, "pipeline\x00%d\x00%s\x00", len(entry.Pipeline), entry.Pipeline)
	fmt.Fprintf(digest, "compiler\x00%x\x00", entry.Compiler)
	fmt.Fprintf(digest, "deps\x00%d\x00", len(entry.Deps))
	for _, dependency := range entry.Deps {
		fmt.Fprintf(digest, "dep\x00%d\x00%s\x00%x\x00", len(dependency.Path), dependency.Path, dependency.Transitive)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// functionCacheBudget is how much disk a function cache directory may hold
// before the least recently used units are evicted. See [cachefile.Trim].
//
// Sized from the corpus. One program's units are 16.9 MB in 45 files for
// fmt_sprintf and 55 MB in 156 files for the http/tls program; a package's key
// does not mention the program, so compiling the whole 406-program corpus with
// one compiler converges on the union of their closures rather than on the sum,
// which is the http/tls figure plus the packages nothing else reaches -- call it
// 60 MB for a full corpus fill against one compiler.
//
// A gibibyte is therefore about sixteen full corpus fills, which is the number
// that matters: what multiplies the cache is not how much a user builds but how
// often the compiler binary changes, because its hash is a clause of the key. Six
// GB of stale generations on a shared box was the failure this bound exists to
// prevent, and sixteen is a comfortable margin for anyone whose working set is
// one compiler.
//
// A variable rather than a constant so that a test can hold the whole trigger --
// compile, write, evict -- against a budget it can reach in seconds instead of
// the sixteen corpus fills it would take to reach this one. Nothing but a test
// assigns to it. See TestTheCacheStaysUnderItsBudgetAcrossGenerations.
var functionCacheBudget int64 = 1 << 30

// functionCacheDefaultOn is whether a compile that has been told nothing uses the
// cache. It is off in the library and turned on by cmd/goc, and that asymmetry is
// the design decision this cache's default rests on. See
// [UseFunctionCacheByDefault].
var functionCacheDefaultOn bool

// UseFunctionCacheByDefault turns the per-function cache on for compiles that
// name no cache directory. cmd/goc calls it; nothing else should.
//
// The default has to differ by caller, and the reason is clause 9 of the key: the
// compiler binary's own hash, which is what covers every compiler change without
// anyone having to remember a version number. It is exactly right for a released
// binary compiling a user's program -- the binary does not move, so the cache
// hits -- and exactly wrong inside `go test`, which builds a fresh test binary
// for every package under test. A cache on by default there would write a
// complete set of package units per test binary and read none of them back: all
// of the cost of filling a cache and none of the benefit, on the tree's own
// suite, forever.
//
// So the compiler turns it on and the library does not. `goc file.go` gets a
// cache; goc.Compile called in process gets one only if it asks, with
// CG12_FUNC_CACHE. A gate that wants the old behaviour from the binary has
// CG12_NOCACHE=1, which still turns off everything.
func UseFunctionCacheByDefault() { functionCacheDefaultOn = true }

// functionCacheDirectory is where per-package units live.
//
//	CG12_NOCACHE=1        nothing is read and nothing is written, whatever else
//	                      is set. The switch the merge gates depend on, checked
//	                      in internal/cachefile so no cache can forget it.
//	CG12_FUNC_CACHE=off   this cache off, the others untouched.
//	CG12_FUNC_CACHE=auto  the default location, whatever the caller's default is.
//	CG12_FUNC_CACHE=<dir> that directory.
//	unset                 the default location if the caller turned the cache on
//	                      (see [UseFunctionCacheByDefault]), otherwise off.
//
// The default location is os.UserCacheDir()/cg12/function-cache, which is the
// shape cmd/goc/packcache.go's packCacheDirectory already had; a box with no user
// cache directory gets no cache rather than an error.
func functionCacheDirectory() string {
	if cachefile.Disabled() {
		return ""
	}
	switch setting := os.Getenv("CG12_FUNC_CACHE"); {
	case setting == "off":
		return ""
	case setting == "":
		if !functionCacheDefaultOn {
			return ""
		}
		return cachefile.Directory("", "function-cache")
	case setting == "auto":
		return cachefile.Directory("", "function-cache")
	default:
		return cachefile.Directory("CG12_FUNC_CACHE", "function-cache")
	}
}

// readPackageCacheUnit returns the stored unit for an entry's key, if there is
// one and it decodes.
func readPackageCacheUnit(directory string, entry *FunctionCacheEntry) *packageCacheUnit {
	contents, found := cachefile.Read(directory, packageCacheKeyDigest(entry), ".gocfn")
	if !found {
		return nil
	}
	unit, err := decodePackageCacheUnit(contents)
	if err != nil {
		// A unit that does not decode is a miss, not a failure. The content digest
		// in the format is what makes that safe: a truncated or spliced file fails
		// the digest rather than decoding into something plausible.
		return nil
	}
	return unit
}

// writePackageCacheUnit stores a unit under its key.
func writePackageCacheUnit(directory string, unit *packageCacheUnit) error {
	return cachefile.Write(directory, packageCacheKeyDigest(unit.Entry), ".gocfn", unit.encode())
}

// ---------------------------------------------------------------------------
// The file format
// ---------------------------------------------------------------------------

// encode writes a package unit. The layout is magic, version, a sha256 of
// everything after it, then the key and the declarations.
//
// The digest is here for the same reason ir/binary.go grew one: a magic tag says
// "this is a cg12 unit" and a version byte says "of a vintage I can read", and
// neither says "and these are the bytes that were written". A cache whose stale
// hit is a wrong binary cannot rely on a truncated write happening to fail some
// later structural check.
func (u *packageCacheUnit) encode() []byte {
	body := &unitWriter{}
	entry := u.Entry
	body.varint(uint64(entry.UnitVersion))
	body.str(entry.Name)
	body.str(entry.Package)
	body.digest(entry.Source)
	body.str(entry.Target)
	body.boolean(entry.Optimize)
	body.str(entry.TextLayout)
	body.str(entry.Pipeline)
	body.digest(entry.Compiler)
	body.varint(uint64(len(entry.Deps)))
	for _, dependency := range entry.Deps {
		body.str(dependency.Path)
		body.digest(dependency.Transitive)
	}
	body.varint(uint64(len(u.order)))
	for _, symbol := range u.order {
		declaration := u.Decls[symbol]
		body.str(declaration.Decl)
		body.str(declaration.Symbol)
		body.varint(uint64(len(declaration.Files)))
		for _, file := range declaration.Files {
			body.str(file)
		}
		body.sequence(declaration.cachedSequence)
	}
	names := u.artifactNames()
	body.varint(uint64(len(names)))
	for _, symbol := range names {
		body.str(symbol)
		body.sequence(u.Artifacts[symbol].cachedSequence)
	}

	out := make([]byte, 0, len(packageUnitMagic)+1+sha256.Size+len(body.buf))
	out = append(out, packageUnitMagic...)
	out = append(out, byte(packageUnitVersion))
	sum := sha256.Sum256(body.buf)
	out = append(out, sum[:]...)
	return append(out, body.buf...)
}

func decodePackageCacheUnit(data []byte) (*packageCacheUnit, error) {
	header := len(packageUnitMagic) + 1 + sha256.Size
	if len(data) < header || string(data[:len(packageUnitMagic)]) != packageUnitMagic {
		return nil, errors.New("goc: not a function-cache unit")
	}
	if version := data[len(packageUnitMagic)]; version != packageUnitVersion {
		return nil, fmt.Errorf("goc: function-cache unit version %d, want %d", version, packageUnitVersion)
	}
	body := data[header:]
	sum := sha256.Sum256(body)
	if string(sum[:]) != string(data[len(packageUnitMagic)+1:header]) {
		return nil, errors.New("goc: function-cache unit fails its content digest")
	}
	reader := &unitReader{buf: body}
	entry := &FunctionCacheEntry{}
	entry.UnitVersion = int(reader.varint())
	entry.Name = reader.str()
	entry.Package = reader.str()
	entry.Source = reader.digest()
	entry.Target = reader.str()
	entry.Optimize = reader.boolean()
	entry.TextLayout = reader.str()
	entry.Pipeline = reader.str()
	entry.Compiler = reader.digest()
	for count := reader.varint(); count > 0; count-- {
		path := reader.str()
		entry.Deps = append(entry.Deps, PackageDependency{Path: path, Transitive: reader.digest()})
	}
	unit := newPackageCacheUnit(entry)
	for count := reader.varint(); count > 0; count-- {
		declaration := &cachedDeclaration{Decl: reader.str(), Symbol: reader.str()}
		for files := reader.varint(); files > 0; files-- {
			declaration.Files = append(declaration.Files, reader.str())
		}
		declaration.cachedSequence = reader.sequence()
		unit.add(declaration)
	}
	for count := reader.varint(); count > 0; count-- {
		artifact := &cachedArtifact{Symbol: reader.str()}
		artifact.cachedSequence = reader.sequence()
		unit.Artifacts[artifact.Symbol] = artifact
	}
	if reader.err != nil {
		return nil, reader.err
	}
	return unit, nil
}

type unitWriter struct{ buf []byte }

func (w *unitWriter) sequence(sequence cachedSequence) {
	w.blob(sequence.Unit)
	w.varint(uint64(len(sequence.Refs)))
	for _, reference := range sequence.Refs {
		w.str(reference.Symbol)
		w.varint(uint64(reference.Funcs))
		w.varint(uint64(reference.Data))
		w.varint(uint64(reference.Types))
	}
	w.varint(uint64(len(sequence.Interns)))
	for _, note := range sequence.Interns {
		w.buf = append(w.buf, byte(note.Kind))
		w.str(note.Key)
		w.str(note.Value)
	}
}

func (w *unitWriter) varint(value uint64) {
	w.buf = binary.AppendUvarint(w.buf, value)
}

func (w *unitWriter) blob(value []byte) {
	w.varint(uint64(len(value)))
	w.buf = append(w.buf, value...)
}

func (w *unitWriter) str(value string) {
	w.varint(uint64(len(value)))
	w.buf = append(w.buf, value...)
}

func (w *unitWriter) boolean(value bool) {
	if value {
		w.buf = append(w.buf, 1)
		return
	}
	w.buf = append(w.buf, 0)
}

func (w *unitWriter) digest(value CacheDigest) { w.buf = append(w.buf, value[:]...) }

type unitReader struct {
	buf []byte
	pos int
	err error
}

func (r *unitReader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *unitReader) take(n int) []byte {
	if n < 0 || r.pos+n > len(r.buf) {
		r.fail(errors.New("goc: truncated function-cache unit"))
		r.pos = len(r.buf)
		return nil
	}
	out := r.buf[r.pos : r.pos+n]
	r.pos += n
	return out
}

func (r *unitReader) byte() byte {
	out := r.take(1)
	if len(out) == 0 {
		return 0
	}
	return out[0]
}

func (r *unitReader) varint() uint64 {
	value, read := binary.Uvarint(r.buf[min(r.pos, len(r.buf)):])
	if read <= 0 {
		r.fail(errors.New("goc: truncated function-cache unit"))
		r.pos = len(r.buf)
		return 0
	}
	r.pos += read
	return value
}

func (r *unitReader) blob() []byte { return r.take(int(r.varint())) }

func (r *unitReader) str() string { return string(r.take(int(r.varint()))) }

func (r *unitReader) boolean() bool { return r.byte() != 0 }

func (r *unitReader) digest() CacheDigest {
	var out CacheDigest
	copy(out[:], r.take(len(out)))
	return out
}

func (r *unitReader) sequence() cachedSequence {
	sequence := cachedSequence{Unit: r.blob()}
	for count := r.varint(); count > 0; count-- {
		sequence.Refs = append(sequence.Refs, artifactReferenceRecord{
			Symbol: r.str(),
			Funcs:  uint32(r.varint()),
			Data:   uint32(r.varint()),
			Types:  uint32(r.varint()),
		})
	}
	for count := r.varint(); count > 0; count-- {
		kind := internKind(r.byte())
		sequence.Interns = append(sequence.Interns, internNote{Kind: kind, Key: r.str(), Value: r.str()})
	}
	return sequence
}

// ---------------------------------------------------------------------------
// Encoding one declaration's delta
// ---------------------------------------------------------------------------

// encodeDeclarationDelta serialises the functions, data and aggregate types one
// declaration added to a module.
//
// The hard part is SrcPos.File, which is a 1-based index into Module.Files. Unit
// A's index 3 is not the program's index 3, and a cross-compilation unit cannot
// do what the memoiser does and simply carry no file table. So the delta is
// encoded against a file table of its own, built in first-use order, and the
// merge maps that table back into whatever the receiving module's indices are.
//
// The renumbering is applied to the live functions and then undone, rather than
// done on a copy. It is a bijection, so undoing it is exact; copying every
// function to encode it would double the cost of filling the cache, and filling
// the cache is already the slow side.
func encodeDeclarationDelta(owner *ir.Module, funcs []*ir.Func, data []*ir.Data, types []*ir.AggType) ([]byte, error) {
	forward := map[uint32]uint32{}
	var files []string
	visit := func(apply func(*ir.SrcPos)) {
		seen := map[*ir.InlineSite]bool{}
		for _, function := range funcs {
			for _, block := range function.Blocks {
				apply(&block.Pos)
				for index := range block.Instrs {
					instruction := &block.Instrs[index]
					apply(&instruction.Pos)
					for site := instruction.Inl; site != nil && !seen[site]; site = site.Parent {
						seen[site] = true
						apply(&site.Call)
					}
				}
			}
		}
	}
	visit(func(pos *ir.SrcPos) {
		if pos.File == 0 {
			return
		}
		index, known := forward[pos.File]
		if !known {
			files = append(files, owner.FileName(pos.File))
			index = uint32(len(files))
			forward[pos.File] = index
		}
		pos.File = index
	})
	scratch := &ir.Module{Funcs: funcs, Data: data, Types: types, Files: files}
	encoded, err := scratch.MarshalBinary()

	reverse := make(map[uint32]uint32, len(forward))
	for original, renumbered := range forward {
		reverse[renumbered] = original
	}
	visit(func(pos *ir.SrcPos) {
		if pos.File == 0 {
			return
		}
		pos.File = reverse[pos.File]
	})
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

// FunctionCacheStats is what one compile did with the cache, for a caller that
// wants to report a hit rate rather than infer one from a stopwatch.
type FunctionCacheStats struct {
	// Packages and PackagesHit count packages offered to the cache and packages
	// that had a usable unit.
	Packages    int
	PackagesHit int
	// Declarations, Hits and Misses count source declarations the lowering loop
	// walked. Misses includes declarations no cache could ever hold.
	Declarations int
	Hits         int
	Misses       int
	// Functions is how many ir.Funcs came out of cached units.
	Functions int
	// Artifacts is how many interned artifact definitions this compile recorded,
	// and ArtifactsReplayed how many it spliced out of units. The first is the
	// duplication design (a) pays; the second is the work the old scheme was
	// silently leaving to whatever program happened to mint them.
	Artifacts         int
	ArtifactsReplayed int
	// Instructions is those functions' weight in IR instructions, which is the
	// denominator BUILD_CACHE.md's projection is quoted against.
	Instructions int
	// TotalInstructions is every lowered function's weight, cached or not.
	TotalInstructions int
	// Reasons counts why entries were refused, by FunctionCacheEntry.Valid's
	// wording, so a surprising miss rate names its own cause.
	Reasons map[string]int
	// Wrote is how many package files this compile stored.
	Wrote int

	// Lowering is the wall time of the declaration loop -- lowering the misses and
	// splicing the hits. It is the stage the cache is about, and the only stage a
	// hit can shorten.
	Lowering time.Duration
	// Replay is the part of Lowering spent decoding units and merging them, and
	// Encode is the part spent serialising deltas for the units this compile will
	// write. The two are what separates the achieved saving from the projected one:
	// the projection priced a hit at zero, and a hit costs a decode.
	Replay time.Duration
	Encode time.Duration
	// Key is the wall time spent computing the compile identity: hashing every
	// source file of the closure and the compiler binary. It is paid whether or not
	// anything hits, so a cache that never hits is this much slower than none.
	Key time.Duration
}

func (s *FunctionCacheStats) reason(text string) {
	if s.Reasons == nil {
		s.Reasons = map[string]int{}
	}
	s.Reasons[text]++
}

// String is a one-line summary for a test log or a build's -v output.
func (s FunctionCacheStats) String() string {
	share := 0.0
	if s.TotalInstructions > 0 {
		share = 100 * float64(s.Instructions) / float64(s.TotalInstructions)
	}
	summary := fmt.Sprintf("function cache: %d/%d packages, %d/%d declarations, %d functions, %d/%d artifacts recorded/replayed, %.1f%% of lowered IR, %d files written; lowering %s (replay %s, encode %s), key %s",
		s.PackagesHit, s.Packages, s.Hits, s.Declarations, s.Functions, s.Artifacts, s.ArtifactsReplayed, share, s.Wrote,
		s.Lowering.Round(time.Millisecond), s.Replay.Round(time.Millisecond),
		s.Encode.Round(time.Millisecond), s.Key.Round(time.Millisecond))
	if len(s.Reasons) == 0 {
		return summary
	}
	reasons := make([]string, 0, len(s.Reasons))
	for text, count := range s.Reasons {
		reasons = append(reasons, fmt.Sprintf("%s x%d", text, count))
	}
	sort.Strings(reasons)
	return summary + " (" + strings.Join(reasons, "; ") + ")"
}

// lastFunctionCacheStats is where a compile leaves its statistics for a test or a
// driver to read. It is process-global and guarded by nothing, because the only
// callers are a single-compile process and a test that compiles one program at a
// time; a caller running two compiles at once gets the last one to finish and is
// told so here rather than finding out.
var lastFunctionCacheStats FunctionCacheStats

// LastFunctionCacheStats returns what the most recent compile in this process did
// with the function cache.
func LastFunctionCacheStats() FunctionCacheStats { return lastFunctionCacheStats }

// FunctionCacheEnabled reports whether a compile in this process would use the
// per-function cache at all.
func FunctionCacheEnabled() bool { return functionCacheDirectory() != "" }

// debugFunctionCache is the one-line report GOC_DEBUG_FUNCCACHE=1 prints.
func debugFunctionCache(stats FunctionCacheStats) {
	if os.Getenv("GOC_DEBUG_FUNCCACHE") == "" {
		return
	}
	fmt.Fprintln(os.Stderr, "goc: "+stats.String())
}

// ---------------------------------------------------------------------------
// Opening a compile's cache
// ---------------------------------------------------------------------------

// openFunctionCache decides whether this compile may use the per-function cache,
// and if so computes its key.
//
// The compiles it declines, and why each one:
//
//   - CG12_NOCACHE=1, or no cache directory. The merge gates depend on that
//     switch producing a build that reads nothing and writes nothing, and it is
//     checked in internal/cachefile so that no cache can be added that forgets it.
//   - A non-executable compile. It never imports the runtime, so its unit map is
//     empty and there is no imported package to cache.
//   - A test-package or external-test-package compile. The loader selects extra
//     files for those, and unitSourceDigest hashes the files the loader parsed --
//     so the key would be right, but sharing a directory with ordinary compiles
//     buys nothing and the shared source world already refuses to be shared with
//     them for the same reason.
//   - A runtime-split compile, either half. Both rewrite the finished module --
//     finishRuntimeModule subtracts definitions, finishProgramModule adds the
//     ones the pack left out -- and this stage has not measured a cached build
//     through either.
//   - A coverage build, which instruments the runtime after lowering.
//   - A -m build. The escape diagnostic is accumulated while a function is
//     lowered; a replayed function is not lowered, so the report would silently
//     cover a subset of the program.
//
// The key it computes is [loadedCompileIdentity]'s, with Optimize false. -O is
// not a clause of THIS cache, and the reason is the answer in
// goc/functionoptimiser_test.go: what is stored is the lowered function, and the
// optimiser runs over the assembled module after the merge, so a unit written by
// a -O build and one written without it are the same bytes. A cache of
// post-optimiser IR would need the clause, and would need a great deal more
// besides.
func openFunctionCache(target Target, options compileOptions, loader *sourceLoader, fset *token.FileSet, pkg *types.Package, name string, src []byte, journal *internJournal) *functionCache {
	if journal == nil {
		return newFunctionCache(nil, "", fset)
	}
	started := time.Now()
	identity, err := loadedCompileIdentity(string(target), false, loader, fset, pkg, name, src)
	elapsed := time.Since(started)
	if err != nil {
		// A key that cannot be computed is a compile without a cache, not a failed
		// compile. The commonest cause is a source file edited between the parse and
		// the hash, which is a race the compile should survive.
		return newFunctionCache(nil, "", fset)
	}
	cache := newFunctionCache(identity, pkg.Path(), fset)
	cache.stats.Key = elapsed
	return cache
}

// newInternJournal decides, from the compile's shape alone, whether lowering
// should be journalled at all.
//
// It is separate from [openFunctionCache] and answered much earlier because of
// where interned artifacts come from. Global initializers are lowered before the
// declaration loop starts, and they mint the descriptors of every global's type --
// which are exactly the artifacts the loop's declarations then reference. A
// journal that started at the loop would see those references as hits with no
// definition anywhere, and every declaration making one would have to be refused.
// So the journal starts before the first thing that can lower, and the cache
// attaches to it once the closure is loaded and its key is knowable.
//
// The clauses are [openFunctionCache]'s, minus the ones that need the loader.
func newInternJournal(options compileOptions) *internJournal {
	usable := options.executable &&
		len(options.testPackages) == 0 &&
		len(options.externalTestPackages) == 0 &&
		options.runtimeSplit == nil &&
		options.runtimeCoverage == nil &&
		opt.EscapeDiagLevel() < 1
	if !usable || functionCacheDirectory() == "" {
		return nil
	}
	return &internJournal{}
}
