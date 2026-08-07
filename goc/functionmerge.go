package goc

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"time"

	"github.com/evanphx/cg12/internal/cachefile"
	"github.com/evanphx/cg12/ir"
)

// This file is the merge: how a decoded unit becomes part of the module being
// compiled, and how the lowering loop decides per declaration whether to lower it
// or to splice it.
//
// The three hazards, and what each one is answered with.
//
//  1. SrcPos.File is a 1-based index into Module.Files (ir/pos.go), so unit A's
//     index 3 is not the program's index 3. The memoiser sidesteps this by never
//     carrying a file table; a cross-compilation unit cannot, because it has to be
//     readable by a compile that never saw the module it was written from. Each
//     unit is encoded against a private file table and remapped on the way in, and
//     the file names the declaration *appended* are replayed first and in order,
//     so the warm module's file table is identical to the cold one's rather than
//     merely equivalent.
//
//  2. AggType interning across units. ir/binary.go allocates a fresh *AggType per
//     decoded reference, and nothing outside the encoder compares them by pointer,
//     so duplicates are wasteful rather than wrong. They are still not free: the
//     encoded module stops matching the cold one, which is the comparison a
//     correctness argument is easiest to make with. So the merge interns them
//     structurally, which reproduces exactly the sharing gen.goABITypes produces on
//     a cold compile -- the aggregate's name is content-derived from the same key
//     that table is keyed on, so two aggregates share a name if and only if the
//     cold compile would have shared the pointer.
//
//  3. ir.Data type descriptors collide by name. Both sides mint the same
//     content-derived `.goc.runtime.type.<sha8>` and ir has no merge-time content
//     check. runtimepack.Manifest.DataDigests does exactly this at the object
//     level and is the model: a second definition of a name is accepted only if it
//     is byte-identical to the first, and a mismatch is a hard error naming the
//     symbol. With interned artifacts carried by the units that reference them
//     (see the artifact journal in goc/functionstore.go) this is no longer an
//     unreachable check but the working half of the merge: two units that both
//     name `.goc.type.time.Time.<sha8>` agree byte for byte, the second copy is
//     dropped, and a disagreement is a bug that names its symbol rather than one
//     side silently winning.

// functionCache is one compile's use of the per-function cache: which units it
// read, what it replayed, and what it will write back.
type functionCache struct {
	directory string
	identity  *CompileIdentity
	root      string
	// fset is the compile's FileSet, for turning a declaration into the source
	// position that identifies it.
	fset *token.FileSet

	// units are the packages whose stored unit was read and validated, by import
	// path. A package with no usable unit is absent.
	units map[string]*packageCacheUnit
	// entries are the keys every loaded or writable package is keyed under.
	entries map[string]*FunctionCacheEntry
	// writing are the units this compile will store, by import path.
	writing map[string]*packageCacheUnit
	// replayed names the symbols that came from a unit rather than from lowering,
	// so a symbol is never both.
	replayed map[string]bool
	// recorded names the symbols this compile has already put in a unit. A
	// declaration lowered twice would otherwise be replayed twice.
	recorded map[string]bool

	// aggregates interns AggTypes structurally across units. See hazard 2.
	aggregates map[string]*ir.AggType
	// aggregatesScanned is how much of Module.Types the intern table has already
	// absorbed, so seeding it is amortised rather than quadratic.
	aggregatesScanned int
	// declaredAggregates is Module.Types as a set, for the same reason.
	declaredAggregates map[*ir.AggType]bool
	// interning guards the walk of nested field types against a type that reaches
	// itself. goc does not build one, and a compiler that starts to should get a
	// duplicated type rather than a hang.
	interning map[*ir.AggType]bool
	// data indexes the module's definitions by name for the collision check, and
	// functions and aggregateNames do the same for the other two tables. Together
	// they answer "does this module already define that symbol", which is what
	// decides whether a referenced artifact is spliced or skipped.
	data              map[string]*ir.Data
	dataScanned       int
	functions         map[string]bool
	functionsScanned  int
	aggregateNames    map[string]bool
	aggregateNamesRun int

	// artifacts are the interned artifacts this compile knows a definition of, by
	// head symbol: the ones its own lowering minted and the ones it read out of a
	// unit. A declaration may only be recorded if every artifact it references is
	// in here, which is what makes a written unit self-sufficient.
	artifacts map[string]*cachedArtifact
	// replaying guards the artifact graph against a cycle. goc does not build one;
	// a unit that claims otherwise should be refused rather than hang.
	replaying map[string]bool

	// pointerKeys is the gen.runtimeTypes information a unit can carry: for each
	// type a cached declaration asked for a descriptor of, the key of the pointer
	// to it. populateRuntimePointerTypes is the only reader of runtimeTypes and
	// that is the only thing it asks.
	pointerKeys map[string]string

	stats FunctionCacheStats
}

// newFunctionCache prepares a compile's cache. A zero directory is the disabled
// state: every method below is a no-op and the compile is the compile it was.
//
// It is never nil, because the lowering loop counts declarations through it and a
// nil receiver cannot hold a counter. Disabled and absent are the same thing to
// every caller, so they are the same thing here.
func newFunctionCache(identity *CompileIdentity, root string, fset *token.FileSet) *functionCache {
	directory := functionCacheDirectory()
	if identity == nil {
		directory = ""
	}
	return &functionCache{
		directory:  directory,
		identity:   identity,
		root:       root,
		fset:       fset,
		units:      map[string]*packageCacheUnit{},
		entries:    map[string]*FunctionCacheEntry{},
		writing:    map[string]*packageCacheUnit{},
		replayed:   map[string]bool{},
		recorded:   map[string]bool{},
		aggregates: map[string]*ir.AggType{},

		declaredAggregates: map[*ir.AggType]bool{},
		interning:          map[*ir.AggType]bool{},
		data:               map[string]*ir.Data{},
		functions:          map[string]bool{},
		aggregateNames:     map[string]bool{},
		artifacts:          map[string]*cachedArtifact{},
		replaying:          map[string]bool{},
		pointerKeys:        map[string]string{},
	}
}

// entryFor is the key for one package, computed once per compile.
//
// A package with no identity, or one the loader resolved from export data rather
// than source, has no unit: it contributes no lowered function, so there would be
// nothing to store.
func (c *functionCache) entryFor(path string) *FunctionCacheEntry {
	if entry, known := c.entries[path]; known {
		return entry
	}
	c.entries[path] = nil
	if path == "" || path == c.root {
		return nil
	}
	identity, known := c.identity.Packages[path]
	if !known || !identity.FromSource {
		return nil
	}
	entry, err := NewFunctionCacheEntry("", path, c.identity)
	if err != nil {
		return nil
	}
	c.entries[path] = entry
	return entry
}

// unitFor reads and validates one package's stored unit, once per compile.
func (c *functionCache) unitFor(path string) *packageCacheUnit {
	if unit, known := c.units[path]; known {
		return unit
	}
	c.units[path] = nil
	entry := c.entryFor(path)
	if entry == nil {
		return nil
	}
	c.stats.Packages++
	stored := readPackageCacheUnit(c.directory, entry)
	if stored == nil {
		c.stats.reason("no stored unit")
		return nil
	}
	// The file was found by a content address of the key, so this can only fail if
	// two different keys collided on one sha256 or if the file on disk is not the
	// file its name claims. Checking anyway is what makes the second case a miss
	// rather than a wrong binary, and Valid names the clause so a surprising
	// refusal explains itself.
	if valid, reason := stored.Entry.Valid(c.identity); !valid {
		c.stats.reason(reason)
		return nil
	}
	if stored.Entry.Package != path {
		c.stats.reason("stored unit is for another package")
		return nil
	}
	c.units[path] = stored
	c.stats.PackagesHit++
	// Every artifact this file carries is a definition this compile now knows,
	// whether or not a declaration of this package turns out to need it. A
	// declaration read from here and carried forward unreplayed into the file this
	// compile writes still names them, so they have to be resolvable then.
	for symbol, artifact := range stored.Artifacts {
		if c.artifacts[symbol] == nil {
			c.artifacts[symbol] = artifact
		}
	}
	return stored
}

// cachedHit is one usable stored declaration and the file it came out of. The
// file travels with it because replaying a declaration means resolving the
// artifacts it references, and those live in the same file.
type cachedHit struct {
	unit        *packageCacheUnit
	declaration *cachedDeclaration
}

// lookup returns the stored delta for one declaration, if the cache has one it
// may use.
func (c *functionCache) lookup(function functionDecl) *cachedHit {
	if c.directory == "" || !cacheableDeclaration(function) {
		return nil
	}
	unit := c.unitFor(function.pkg.Path())
	if unit == nil {
		return nil
	}
	key := declarationKey(c.fset, function)
	declaration := unit.Decls[key]
	if declaration == nil || c.replayed[key] || c.recorded[key] {
		return nil
	}
	return &cachedHit{unit: unit, declaration: declaration}
}

// declarationKey identifies one declaration within its package. See
// [cachedDeclaration.Decl] for why it is a source position and not a symbol.
func declarationKey(fset *token.FileSet, function functionDecl) string {
	position := fset.Position(function.decl.Name.Pos())
	return fmt.Sprintf("%s:%d:%d", TrimPath(position.Filename), position.Line, position.Column)
}

// cacheableDeclaration is the part of the classification that can be decided
// before anything is lowered.
//
// An instantiation is excluded because which instantiations exist is a
// whole-program fact -- the type arguments come from the importer -- so it is not
// part of its package's unit even though it lowers deterministically from its
// origin and arguments. See CacheUnitInstantiation. Both tests are needed: the
// front end sets a symbol on the declaration only when it had to compute one,
// which for a generic is exactly when there are type arguments, but a closure or
// method value derived from an instantiation arrives with the symbol alone.
func cacheableDeclaration(function functionDecl) bool {
	return function.decl != nil && function.decl.Name != nil && function.pkg != nil &&
		len(function.typeArguments) == 0 && !IsGenericInstanceSymbol(function.symbol)
}

// ---------------------------------------------------------------------------
// Recording
// ---------------------------------------------------------------------------

// declarationMark is where a module and a journal stood before one declaration
// was lowered. Everything after it is that declaration's delta.
type declarationMark struct {
	funcs, data, types, files int
	notes                     int
	events                    int
	dependent                 int
}

func markDeclaration(module *ir.Module, journal *internJournal) declarationMark {
	mark := journal.mark(module)
	journal.beginFileScope()
	return mark
}

// loweringScope is one artifact's extent, or one declaration's, reconstructed
// from the journal's events. The children are in the order the scope touched
// them; a child that minted its artifact carries the artifact's own scope, and a
// child that only referenced one carries nothing but the position.
type loweringScope struct {
	symbol   string
	begin    artifactEvent
	end      artifactEvent
	children []loweringChild
}

type loweringChild struct {
	at     artifactEvent
	minted *loweringScope
}

// buildLoweringScope turns the flat event list into the tree the calls made. It
// returns nil for an unbalanced list, which would mean a mint site opened a scope
// and returned without closing it; a scope that cannot be read is a declaration
// that is not recorded, not a compile that fails.
func buildLoweringScope(events []artifactEvent, begin, end artifactEvent) *loweringScope {
	root := &loweringScope{begin: begin, end: end}
	stack := []*loweringScope{root}
	for _, event := range events {
		top := stack[len(stack)-1]
		switch event.Kind {
		case artifactBegin:
			minted := &loweringScope{symbol: event.Symbol, begin: event}
			top.children = append(top.children, loweringChild{at: event, minted: minted})
			stack = append(stack, minted)
		case artifactEnd:
			if len(stack) == 1 {
				return nil
			}
			top.end = event
			stack = stack[:len(stack)-1]
		case artifactReference:
			top.children = append(top.children, loweringChild{at: event})
		}
	}
	if len(stack) != 1 {
		return nil
	}
	return root
}

// collectedScope is one scope's contribution separated from its children's: the
// items it appended itself, where each referenced artifact fell among them, and
// the table entries it made directly.
type collectedScope struct {
	funcs   []*ir.Func
	data    []*ir.Data
	types   []*ir.AggType
	refs    []artifactReferenceRecord
	interns []internNote
	// missing is the first artifact this scope referenced that no definition is
	// known for. The scope is still walked to the end when one turns up: the
	// artifacts it goes on to mint are definitions other declarations will need,
	// and dropping them because this one is unusable would make those unusable too.
	missing string
}

// collectScope walks one scope, registering the definition of every artifact it
// minted and checking that every artifact it merely referenced already has one.
//
// The second half is the whole point. A reference with no definition anywhere is
// a delta that would dangle in any program that does not happen to contain
// whatever minted it, so it is refused here -- as a lost cache hit -- rather than
// written out as a unit that fails to link. It happens for artifacts minted before
// the journal existed at all, which is why [newInternJournal] starts it as early
// as it does.
func (c *functionCache) collectScope(module *ir.Module, notes []internNote, scope *loweringScope) collectedScope {
	var collected collectedScope
	funcs, data, types, note := scope.begin.Funcs, scope.begin.Data, scope.begin.Types, scope.begin.Notes
	for _, child := range scope.children {
		collected.funcs = append(collected.funcs, module.Funcs[funcs:child.at.Funcs]...)
		collected.data = append(collected.data, module.Data[data:child.at.Data]...)
		collected.types = append(collected.types, module.Types[types:child.at.Types]...)
		collected.interns = append(collected.interns, notes[note:child.at.Notes]...)
		known := true
		if child.minted == nil {
			known = c.artifacts[child.at.Symbol] != nil
			funcs, data, types, note = child.at.Funcs, child.at.Data, child.at.Types, child.at.Notes
		} else {
			var missing string
			known, missing = c.recordArtifact(module, notes, child.minted)
			if missing != "" && collected.missing == "" {
				collected.missing = missing
			}
			funcs, data, types, note = child.minted.end.Funcs, child.minted.end.Data, child.minted.end.Types, child.minted.end.Notes
		}
		if !known {
			// Either a mint that appended nothing, or a reference to something minted
			// before this compile journalled anything. The first contributed nothing
			// for a replay to put back; the second is what missing records. The
			// reference is dropped either way, so a stored sequence never names a
			// definition its file will not carry.
			if child.minted == nil && collected.missing == "" {
				collected.missing = child.at.Symbol
			}
			continue
		}
		collected.refs = append(collected.refs, artifactReferenceRecord{
			Symbol: child.at.Symbol,
			Funcs:  uint32(len(collected.funcs)),
			Data:   uint32(len(collected.data)),
			Types:  uint32(len(collected.types)),
		})
	}
	collected.funcs = append(collected.funcs, module.Funcs[funcs:scope.end.Funcs]...)
	collected.data = append(collected.data, module.Data[data:scope.end.Data]...)
	collected.types = append(collected.types, module.Types[types:scope.end.Types]...)
	collected.interns = append(collected.interns, notes[note:scope.end.Notes]...)
	return collected
}

// encodeScope serialises what collectScope separated out.
func (c *functionCache) encodeScope(module *ir.Module, collected collectedScope) (cachedSequence, error) {
	started := time.Now()
	encoded, err := encodeDeclarationDelta(module, collected.funcs, collected.data, collected.types)
	c.stats.Encode += time.Since(started)
	if err != nil {
		return cachedSequence{}, err
	}
	return cachedSequence{
		Unit:    encoded,
		Refs:    append([]artifactReferenceRecord(nil), collected.refs...),
		Interns: append([]internNote(nil), collected.interns...),
	}, nil
}

// recordArtifact stores one interned artifact's definition, once per compile. It
// reports whether a definition is now known, and names the first artifact the
// definition referenced that none is known for.
//
// A mint that appended nothing is not stored. Two of the sites open their scope
// before they know whether they will emit -- goABIAggregate can find the type has
// no ABI shape, staticInterfacePayload can find the constant will not render --
// and both leave their intern table untouched when that happens, so the symbol is
// minted again on the next call. Storing the empty first attempt would make the
// real definition the one that is dropped.
func (c *functionCache) recordArtifact(module *ir.Module, notes []internNote, scope *loweringScope) (bool, string) {
	if c.artifacts[scope.symbol] != nil {
		return true, ""
	}
	if scope.end.Dependent != scope.begin.Dependent {
		// Minting it consulted a whole-program fact, so the definition is this
		// program's rather than this type's. goc does not do that today -- the one
		// site that consults one is a conversion, not a mint -- and if it starts to,
		// the artifact is refused rather than stored wrong.
		return false, scope.symbol
	}
	collected := c.collectScope(module, notes, scope)
	if collected.missing != "" {
		// The definition is incomplete: it names an artifact this compile cannot
		// carry, and the reference to it was dropped rather than written out
		// dangling. Storing what is left would be storing a definition that is
		// short of what a cold compile produces, so it is not stored, and every
		// declaration that references it is refused for the same reason.
		return false, collected.missing
	}
	if len(collected.funcs)+len(collected.data)+len(collected.types)+len(collected.refs) == 0 {
		return false, ""
	}
	sequence, err := c.encodeScope(module, collected)
	if err != nil {
		return false, scope.symbol
	}
	c.artifacts[scope.symbol] = &cachedArtifact{Symbol: scope.symbol, cachedSequence: sequence}
	c.stats.Artifacts++
	return true, collected.missing
}

// record stores what one lowered declaration added, if a cache may hold it.
//
// The classification runs over everything the declaration itself appended, and
// not over what its artifacts did. An interface-call wrapper is not a unit --
// redirectUnavailableInterfaceCallWrappers chooses its body from the assembled
// program -- but a wrapper reached through an itab is part of that itab's
// definition, and an itab that did not carry its wrappers would name symbols
// nothing defines. Carrying them is safe for the reason the classification exists:
// the redirect is a whole-program pass over the finished module and runs on a warm
// compile exactly as it does on a cold one.
//
// The artifacts are registered even when the declaration is refused. A refused
// declaration still minted definitions an accepted one may reference, and a
// reference whose definition this compile has forgotten costs the accepted
// declaration its unit.
func (c *functionCache) record(module *ir.Module, journal *internJournal, function functionDecl, mark declarationMark) {
	if c.directory == "" {
		return
	}
	scope := buildLoweringScope(journal.events[mark.events:],
		artifactEvent{Funcs: mark.funcs, Data: mark.data, Types: mark.types, Notes: mark.notes},
		journal.here(module))
	if scope == nil {
		c.stats.reason("unbalanced artifact journal")
		return
	}
	collected := c.collectScope(module, journal.notes, scope)
	if !cacheableDeclaration(function) {
		return
	}
	if journal.dependent != mark.dependent {
		// The lowering consulted a fact about the whole program, so what it appended
		// is not a function of its package. See gen.wholeProgramLowering.
		c.stats.reason("lowering used a whole-program fact")
		return
	}
	if collected.missing != "" {
		c.stats.reason("no definition of " + collected.missing)
		return
	}
	path := function.pkg.Path()
	entry := c.entryFor(path)
	if entry == nil {
		return
	}
	key := declarationKey(c.fset, function)
	if c.recorded[key] || c.replayed[key] {
		// Lowered twice in one compile. One delta cannot describe two, and the
		// second replay would emit the first one's functions again, so the pair is
		// dropped rather than half-recorded.
		delete(c.unitBeingWritten(path, entry).Decls, key)
		return
	}
	for _, lowered := range collected.funcs {
		if !ClassifyCacheUnit(lowered).Cacheable() {
			return
		}
	}
	if len(collected.funcs) == 0 {
		// A declaration that lowered to nothing at all. There is no delta to store,
		// and storing an empty one would make the warm compile skip whatever side
		// effect the lowering had that this stage has not accounted for.
		return
	}
	sequence, err := c.encodeScope(module, collected)
	if err != nil {
		return
	}
	c.recorded[key] = true
	c.unitBeingWritten(path, entry).add(&cachedDeclaration{
		Decl:           key,
		Symbol:         collected.funcs[0].Name,
		Files:          touchedFileNames(module, journal.files[mark.files:]),
		cachedSequence: sequence,
	})
}

// attachArtifacts gives a unit about to be written the definitions its
// declarations name, transitively.
//
// This is where the duplication design (a) pays lands: one copy per package file,
// however many of the file's declarations reference it. It is sound for the same
// reason the file's key is: an artifact describes types the package's own source
// mentions, so every package whose source could change its content is already a
// clause of the key the file is stored under.
func (c *functionCache) attachArtifacts(unit *packageCacheUnit) error {
	pending := make([]string, 0, len(unit.Decls))
	for _, declaration := range unit.Decls {
		for _, reference := range declaration.Refs {
			pending = append(pending, reference.Symbol)
		}
	}
	for len(pending) > 0 {
		symbol := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if unit.Artifacts[symbol] != nil {
			continue
		}
		artifact := c.artifacts[symbol]
		if artifact == nil {
			return fmt.Errorf("goc: no definition of %s for %s", symbol, unit.Entry.Package)
		}
		unit.Artifacts[symbol] = artifact
		for _, reference := range artifact.Refs {
			pending = append(pending, reference.Symbol)
		}
	}
	return nil
}

// absorbPrelowering records the artifacts minted before the declaration loop
// started -- by the globals and by the dynamic initializers -- so that a
// declaration referencing one of them has a definition to carry.
//
// Nothing keeps their own items: those are not any declaration's delta and no
// cached compile skips producing them. What is kept is the definition of each
// artifact, which is exactly what a later reference needs.
func (c *functionCache) absorbPrelowering(module *ir.Module, journal *internJournal) {
	if c.directory == "" || journal == nil {
		return
	}
	scope := buildLoweringScope(journal.events, artifactEvent{}, journal.here(module))
	if scope == nil {
		return
	}
	for _, child := range scope.children {
		if child.minted == nil {
			continue
		}
		if _, missing := c.recordArtifact(module, journal.notes, child.minted); missing != "" {
			c.stats.reason("no definition of " + missing)
		}
	}
}

func (c *functionCache) unitBeingWritten(path string, entry *FunctionCacheEntry) *packageCacheUnit {
	unit := c.writing[path]
	if unit == nil {
		unit = newPackageCacheUnit(entry)
		c.writing[path] = unit
	}
	return unit
}

// carryForward copies the declarations this compile replayed into the units it is
// about to write, so that a package whose file was a hit is not rewritten with
// only the handful of declarations that missed.
func (c *functionCache) carryForward() {
	for path, unit := range c.units {
		if unit == nil {
			continue
		}
		writing := c.writing[path]
		if writing == nil {
			continue
		}
		for _, key := range unit.order {
			if _, present := writing.Decls[key]; present {
				continue
			}
			writing.add(unit.Decls[key])
		}
	}
}

// flush writes the units this compile produced.
//
// A package whose stored unit already holds everything this compile lowered is
// not rewritten: rewriting it would cost a few megabytes of I/O for `runtime` to
// produce the file that is already there. A failure to write is not a build
// failure -- the module is already compiled -- so it is counted and dropped.
func (c *functionCache) flush() {
	if c.directory == "" {
		return
	}
	// The one moment the cache is guaranteed to have done its work, and so the one
	// moment it is worth walking the directory to evict. Trim rate-limits itself to
	// once a day per directory.
	cachefile.Trim(c.directory, functionCacheBudget)
	c.carryForward()
	paths := make([]string, 0, len(c.writing))
	for path := range c.writing {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		unit := c.writing[path]
		if len(unit.Decls) == 0 {
			continue
		}
		if stored := c.units[path]; stored != nil && len(stored.Decls) == len(unit.Decls) {
			continue
		}
		if err := c.attachArtifacts(unit); err != nil {
			// A unit whose artifacts cannot all be named is not written. record
			// refuses a declaration whose references it cannot resolve, so this should
			// be unreachable; dropping the file rather than storing a partial one is
			// what keeps it from becoming a program that does not link.
			c.stats.reason(err.Error())
			continue
		}
		sort.Strings(unit.order)
		if err := writePackageCacheUnit(c.directory, unit); err == nil {
			c.stats.Wrote++
		}
	}
}

// ---------------------------------------------------------------------------
// Replaying
// ---------------------------------------------------------------------------

// replay splices one cached declaration into the module being compiled and
// restores the intern-table entries its lowering would have made.
//
// The file names come first, because SrcPos indices resolve against them and
// replaying them in order is what keeps the warm module's file table identical to
// the cold one's rather than merely equivalent. The rest is [replaySequence].
func (c *functionCache) replay(g *gen, hit *cachedHit) error {
	started := time.Now()
	defer func() { c.stats.Replay += time.Since(started) }()
	for _, name := range hit.declaration.Files {
		g.mod.File(name)
	}
	if err := c.replaySequence(g, hit.unit, hit.declaration.Symbol, hit.declaration.cachedSequence); err != nil {
		return err
	}
	c.replayed[hit.declaration.Decl] = true
	return nil
}

// replaySequence splices one declaration's or one artifact's items into the
// module, resolving the artifacts it references at the positions it referenced
// them.
//
// The walk is the whole correctness argument. A cold compile appends a
// declaration's own items in order and, at the point of first reference, the
// definition of each artifact it names; the stored sequence is that same order
// with nothing left out, because record put the artifacts back at their
// positions. Filtering it by what the module already defines is exactly what a
// cold compile of THIS program does, so the two produce the same indices -- which
// matters because Module.Data order is the image's data layout.
func (c *functionCache) replaySequence(g *gen, unit *packageCacheUnit, owner string, sequence cachedSequence) error {
	module := g.mod
	decoded, err := ir.DecodeModuleUnverified(sequence.Unit)
	if err != nil {
		return fmt.Errorf("goc: cached unit for %s: %w", owner, err)
	}
	c.remapFilePositions(module, decoded)

	// The aggregate types are interned structurally before anything is appended,
	// so that a reference from a function reaches the module's copy rather than a
	// second one the decode allocated.
	c.seedAggregates(module)
	canonical := make(map[string]*ir.AggType, len(decoded.Types))
	for _, function := range decoded.Funcs {
		c.internFunctionAggregates(function)
	}
	for index, aggregate := range decoded.Types {
		interned := c.internAggregate(aggregate)
		decoded.Types[index] = interned
		canonical[interned.Name] = interned
	}

	cursor := sequenceCursor{owner: owner}
	for _, reference := range sequence.Refs {
		if err := c.emitOwn(module, decoded, &cursor, int(reference.Funcs), int(reference.Data), int(reference.Types)); err != nil {
			return err
		}
		if err := c.ensureArtifact(g, unit, reference.Symbol); err != nil {
			return err
		}
	}
	if err := c.emitOwn(module, decoded, &cursor, len(decoded.Funcs), len(decoded.Data), len(decoded.Types)); err != nil {
		return err
	}

	// The intern tables last, so the next declaration that wants the same literal
	// finds the symbol this one already put in the module.
	for _, note := range sequence.Interns {
		switch note.Kind {
		case internLiteralData:
			g.literalData[note.Key] = note.Value
		case internContentSymbol:
			g.contentSymbols[note.Key] = note.Value
		case internFunctionDescriptor:
			g.functionDescriptors[note.Key] = note.Value
		case internTypeTag:
			g.typeTags[note.Key] = note.Value
		case internRuntimeType:
			c.pointerKeys[note.Key] = note.Value
		case internInterfaceItab:
			g.interfaceItabs[note.Key] = note.Value
		case internInterfaceCallWrapper:
			g.interfaceCallWrappers[note.Key] = note.Value
		case internGoABIType:
			if aggregate := canonical[note.Value]; aggregate != nil {
				g.goABITypes[note.Key] = aggregate
			}
		case internTypeEqualTarget:
			// The descriptor is either in this sequence -- appended above -- or in one
			// an earlier declaration contributed, so the module is what is searched
			// rather than the unit.
			setRuntimeTypeEqualDescriptor(module, note.Key, note.Value)
		}
	}
	return nil
}

// sequenceCursor is how far through a decoded sequence's own items the walk has
// got. The three counts advance independently because Module.Funcs, Module.Data
// and Module.Types are three lists, and a reference records a position in each.
type sequenceCursor struct {
	owner              string
	funcs, data, types int
}

// emitOwn appends the decoded sequence's own items up to the given positions.
//
// A datum whose name the module already defines is dropped rather than appended,
// after being checked against the definition already there. That is the dedupe
// design (a) needs, and it is the same check runtimepack.Manifest.DataDigests
// makes at the object level: the name is a content hash, so two units that both
// carry it agree, and a disagreement names its symbol instead of one side quietly
// winning.
func (c *functionCache) emitOwn(module *ir.Module, decoded *ir.Module, cursor *sequenceCursor, funcs, data, types int) error {
	for ; cursor.types < types; cursor.types++ {
		aggregate := decoded.Types[cursor.types]
		if c.declaredAggregates[aggregate] {
			continue
		}
		c.declaredAggregates[aggregate] = true
		module.AddType(aggregate)
		c.aggregatesScanned = len(module.Types)
		c.aggregateNames[aggregate.Name] = true
		c.aggregateNamesRun = len(module.Types)
	}
	c.seedData(module)
	for ; cursor.data < data; cursor.data++ {
		datum := decoded.Data[cursor.data]
		existing := c.data[datum.Name]
		if existing == nil {
			c.data[datum.Name] = datum
			module.Data = append(module.Data, datum)
			c.dataScanned = len(module.Data)
			continue
		}
		if err := sameDataDefinition(existing, datum); err != nil {
			return fmt.Errorf("goc: cached unit for %s redefines %s: %w", cursor.owner, datum.Name, err)
		}
	}
	c.seedFunctions(module)
	for ; cursor.funcs < funcs; cursor.funcs++ {
		function := decoded.Funcs[cursor.funcs]
		if c.functions[function.Name] {
			return fmt.Errorf("goc: cached unit for %s redefines function %s", cursor.owner, function.Name)
		}
		c.functions[function.Name] = true
		module.Adopt(function)
		c.functionsScanned = len(module.Funcs)
		c.stats.Functions++
		for _, block := range function.Blocks {
			c.stats.Instructions += len(block.Instrs)
		}
	}
	return nil
}

// ensureArtifact splices one interned artifact's definition into the module, if
// the module does not already have it.
//
// This is ensureTypeTag and its eight siblings, replayed: mint on first
// reference, return the name on every one after. The difference is only where the
// definition comes from -- a unit rather than the type checker, because a warm
// compile has no types.Type for a body it never type-checked.
func (c *functionCache) ensureArtifact(g *gen, unit *packageCacheUnit, symbol string) error {
	artifact := unit.Artifacts[symbol]
	if artifact == nil {
		if artifact = c.artifacts[symbol]; artifact == nil {
			return fmt.Errorf("goc: unit for %s carries no definition of %s", unit.Entry.Package, symbol)
		}
	}
	// Registered before the presence test, not after: a declaration this compile
	// LOWERS may reference the same artifact, and record can only accept it if this
	// compile can name a definition to carry forward.
	if c.artifacts[symbol] == nil {
		c.artifacts[symbol] = artifact
	}
	if c.defines(g.mod, symbol) {
		return nil
	}
	if c.replaying[symbol] {
		return fmt.Errorf("goc: interned artifact %s is defined in terms of itself", symbol)
	}
	c.replaying[symbol] = true
	defer delete(c.replaying, symbol)
	c.stats.ArtifactsReplayed++
	return c.replaySequence(g, unit, symbol, artifact.cachedSequence)
}

// defines reports whether the module already holds the definition an artifact's
// head symbol names. Presence of the head means presence of the whole run: an
// artifact's items are appended by one call, so nothing can have half of one.
func (c *functionCache) defines(module *ir.Module, symbol string) bool {
	c.seedData(module)
	if c.data[symbol] != nil {
		return true
	}
	c.seedFunctions(module)
	if c.functions[symbol] {
		return true
	}
	c.seedAggregateNames(module)
	return c.aggregateNames[symbol]
}

// remapFilePositions rewrites a decoded unit's SrcPos file indices, which address
// the unit's private file table, into the receiving module's.
func (c *functionCache) remapFilePositions(module *ir.Module, decoded *ir.Module) {
	remap := make([]uint32, len(decoded.Files)+1)
	for index, name := range decoded.Files {
		remap[index+1] = module.File(name)
	}
	seen := map[*ir.InlineSite]bool{}
	apply := func(pos *ir.SrcPos) {
		if pos.File == 0 || int(pos.File) >= len(remap) {
			pos.File = 0
			return
		}
		pos.File = remap[pos.File]
	}
	for _, function := range decoded.Funcs {
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

// seedAggregates absorbs the aggregate types the module has gained since the last
// call into the intern table.
func (c *functionCache) seedAggregates(module *ir.Module) {
	for ; c.aggregatesScanned < len(module.Types); c.aggregatesScanned++ {
		aggregate := module.Types[c.aggregatesScanned]
		c.declaredAggregates[aggregate] = true
		c.internAggregate(aggregate)
	}
}

// seedFunctions and seedAggregateNames are seedData for the other two tables.
func (c *functionCache) seedFunctions(module *ir.Module) {
	for ; c.functionsScanned < len(module.Funcs); c.functionsScanned++ {
		c.functions[module.Funcs[c.functionsScanned].Name] = true
	}
}

func (c *functionCache) seedAggregateNames(module *ir.Module) {
	for ; c.aggregateNamesRun < len(module.Types); c.aggregateNamesRun++ {
		c.aggregateNames[module.Types[c.aggregateNamesRun].Name] = true
	}
}

func (c *functionCache) seedData(module *ir.Module) {
	for ; c.dataScanned < len(module.Data); c.dataScanned++ {
		datum := module.Data[c.dataScanned]
		if _, known := c.data[datum.Name]; !known {
			c.data[datum.Name] = datum
		}
	}
}

// internAggregate returns the canonical pointer for a structurally identical
// aggregate type, adopting this one if it is the first of its shape.
//
// The nested field types are canonicalised first, and that is not a refinement:
// ir/binary.go's collectTypes walks Field.Type as well as the top-level
// references, so an aggregate whose *field* type is a second copy of a type the
// module already has encodes one more entry in the type table than the cold
// compile did. That is exactly what the first attempt at this got wrong -- two
// collected types cold against three warm, on a function whose printed IL was
// identical.
func (c *functionCache) internAggregate(aggregate *ir.AggType) *ir.AggType {
	if aggregate == nil || c.interning[aggregate] {
		return aggregate
	}
	c.interning[aggregate] = true
	for index := range aggregate.Fields {
		aggregate.Fields[index].Type = c.internAggregate(aggregate.Fields[index].Type)
	}
	for union := range aggregate.Cases {
		for index := range aggregate.Cases[union] {
			aggregate.Cases[union][index].Type = c.internAggregate(aggregate.Cases[union][index].Type)
		}
	}
	delete(c.interning, aggregate)
	key := aggregateKey(aggregate)
	if existing, known := c.aggregates[key]; known {
		return existing
	}
	c.aggregates[key] = aggregate
	return aggregate
}

// internFunctionAggregates rewrites every aggregate reference in a decoded
// function to the canonical pointer.
//
// The six places a *AggType can hang off a function are ir/binary.go's
// collectTypes' list, and they are read from it rather than rediscovered: the
// encoder already has to know every one of them, and a merge that missed one
// would leave a duplicate type behind in exactly the case the encoder does not.
func (c *functionCache) internFunctionAggregates(function *ir.Func) {
	function.RetAgg = c.internAggregate(function.RetAgg)
	for index := range function.AggregateValues {
		function.AggregateValues[index].Type = c.internAggregate(function.AggregateValues[index].Type)
	}
	for index := range function.ParamGroups {
		function.ParamGroups[index].Type = c.internAggregate(function.ParamGroups[index].Type)
	}
	for _, temporary := range function.Temps {
		if temporary != nil {
			temporary.Agg = c.internAggregate(temporary.Agg)
		}
	}
	for _, block := range function.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			instruction.RetAgg = c.internAggregate(instruction.RetAgg)
			for argument := range instruction.AggArgs {
				instruction.AggArgs[argument] = c.internAggregate(instruction.AggArgs[argument])
			}
			for group := range instruction.ArgGroups {
				instruction.ArgGroups[group].Type = c.internAggregate(instruction.ArgGroups[group].Type)
			}
		}
	}
}

// aggregateKey is a structural spelling of an aggregate type, including its name.
//
// The name is part of it because goc derives an aggregate's name from the same
// key gen.goABITypes is keyed on, so two aggregates share a name exactly when a
// cold compile would have shared the pointer. Interning on structure alone would
// merge two aggregates a cold compile kept apart, and the merged module would stop
// matching the cold one for no gain.
func aggregateKey(aggregate *ir.AggType) string {
	var out strings.Builder
	var write func(*ir.AggType, int)
	writeFields := func(fields []ir.Field, depth int) {
		for _, field := range fields {
			fmt.Fprintf(&out, "f%d,%d,%v;", field.Sub, field.Count, field.Pointer)
			if field.Type != nil {
				write(field.Type, depth+1)
			}
		}
	}
	write = func(aggregate *ir.AggType, depth int) {
		if aggregate == nil || depth > 16 {
			out.WriteString("<>")
			return
		}
		fmt.Fprintf(&out, "[%s|%d|%d|%v|%v|%v|", aggregate.Name, aggregate.Align, aggregate.Size,
			aggregate.Opaque, aggregate.Packed, aggregate.Union)
		writeFields(aggregate.Fields, depth)
		for _, union := range aggregate.Cases {
			out.WriteString("c:")
			writeFields(union, depth)
		}
		out.WriteString("]")
	}
	write(aggregate, 0)
	return out.String()
}

// sameDataDefinition reports whether two definitions of one symbol are the same
// definition.
//
// This is runtimepack.Manifest.DataDigests' check moved to the IR level, and it
// exists because both sides of a merge mint the same content-derived name for a
// type descriptor: `.goc.runtime.type.<sha8>` is derived from the type's key, so a
// cached unit and a freshly lowered function that both describe the same type
// produce the same symbol and ir has nothing that notices. Comparing the
// definitions is what turns "one of them silently wins" into a diagnosis.
func sameDataDefinition(left, right *ir.Data) error {
	if left.Align != right.Align || left.GoTypeLink != right.GoTypeLink || left.Linkage != right.Linkage {
		return fmt.Errorf("alignment, linkage or type-link flag differs")
	}
	if len(left.Items) != len(right.Items) || len(left.PointerWords) != len(right.PointerWords) {
		return fmt.Errorf("%d items and %d pointer words against %d and %d",
			len(left.Items), len(left.PointerWords), len(right.Items), len(right.PointerWords))
	}
	for index := range left.PointerWords {
		if left.PointerWords[index] != right.PointerWords[index] {
			return fmt.Errorf("pointer word %d differs", index)
		}
	}
	for index := range left.Items {
		if !sameDataItem(left.Items[index], right.Items[index]) {
			return fmt.Errorf("item %d differs", index)
		}
	}
	return nil
}

func sameDataItem(left, right ir.DataItem) bool {
	if left.Sub != right.Sub || left.Zero != right.Zero || left.Str != right.Str ||
		left.Sym != right.Sym || left.RelativeTo != right.RelativeTo || left.Off != right.Off ||
		len(left.Ints) != len(right.Ints) || len(left.Flts) != len(right.Flts) {
		return false
	}
	for index := range left.Ints {
		if left.Ints[index] != right.Ints[index] {
			return false
		}
	}
	for index := range left.Flts {
		if left.Flts[index] != right.Flts[index] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// What the rest of the compile asks the cache
// ---------------------------------------------------------------------------

// finishLowering records the denominator the achieved share is quoted against:
// every function the lowering loop produced, cached or not, weighted by IR
// instructions. It is taken here rather than at the end of the compile because
// what comes after the loop -- the interface-method dispatchers, the init tasks,
// the memory helpers -- is generated from the assembled program and is not part
// of what a cache could ever hold.
func (c *functionCache) finishLowering(module *ir.Module, started time.Time) {
	c.stats.Lowering = time.Since(started)
	if c.directory == "" {
		return
	}
	total := 0
	for _, function := range module.Funcs {
		for _, block := range function.Blocks {
			total += len(block.Instrs)
		}
	}
	c.stats.TotalInstructions = total
}

// pointerTypeKeys is what populateRuntimePointerTypes needs: for every type the
// compile emitted a descriptor for, the key of the pointer to it.
//
// Two sources, because a warm compile has two. The live gen.runtimeTypes holds
// the types this compile actually lowered a body for; the replayed notes hold the
// types a cached declaration asked for, which this compile never type-checked and
// so has no types.Type for. Without the second, a program whose only reference to
// `*T` came from a cached function would leave T's descriptor with an empty
// PtrToThis and the warm binary would differ from the cold one.
func (c *functionCache) pointerTypeKeys(fset *token.FileSet, runtimeTypes map[string]types.Type) map[string]string {
	keys := make(map[string]string, len(runtimeTypes)+len(c.pointerKeys))
	for key, value := range runtimeTypes {
		keys[key] = runtimePointerTypeKey(fset, value)
	}
	for key, pointerKey := range c.pointerKeys {
		keys[key] = pointerKey
	}
	return keys
}

// report is what this compile did, for a driver or a test to read back.
func (c *functionCache) report() FunctionCacheStats { return c.stats }
