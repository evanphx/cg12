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
//     symbol. It should be unreachable -- the intern tables are replayed, so
//     nothing should re-mint what a unit already supplied -- which is why it is an
//     error rather than a silent preference for one side.

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
	// data indexes the module's definitions by name for the collision check.
	data        map[string]*ir.Data
	dataScanned int

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
	return stored
}

// lookup returns the stored delta for one declaration, if the cache has one it
// may use.
func (c *functionCache) lookup(function functionDecl) *cachedDeclaration {
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
	return declaration
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
	interns                   int
}

func markDeclaration(module *ir.Module, journal *internJournal) declarationMark {
	return declarationMark{
		funcs:   len(module.Funcs),
		data:    len(module.Data),
		types:   len(module.Types),
		files:   len(module.Files),
		interns: journal.mark(),
	}
}

// record stores what one lowered declaration added, if a cache may hold it.
//
// The classification runs over the whole delta and not just over the declaration
// itself. A declaration's lowering can create an interface-call wrapper on demand
// at a call site, and a wrapper's body is chosen from the assembled program by
// redirectUnavailableInterfaceCallWrappers -- so a delta containing one is not a
// unit, whatever the declaration at the head of it is. Refusing the whole delta
// is the conservative direction: a declaration wrongly excluded is a slower build,
// and one wrongly included is a wrong binary.
func (c *functionCache) record(module *ir.Module, journal *internJournal, function functionDecl, mark declarationMark) {
	if c.directory == "" || !cacheableDeclaration(function) {
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
	funcs := module.Funcs[mark.funcs:]
	for _, lowered := range funcs {
		if !ClassifyCacheUnit(lowered).Cacheable() {
			return
		}
	}
	if len(funcs) == 0 {
		// A declaration that lowered to nothing at all. There is no delta to store,
		// and storing an empty one would make the warm compile skip whatever side
		// effect the lowering had that this stage has not accounted for.
		return
	}
	started := time.Now()
	encoded, err := encodeDeclarationDelta(module, funcs, module.Data[mark.data:], module.Types[mark.types:])
	c.stats.Encode += time.Since(started)
	if err != nil {
		return
	}
	c.recorded[key] = true
	c.unitBeingWritten(path, entry).add(&cachedDeclaration{
		Decl:     key,
		Symbol:   funcs[0].Name,
		NewFiles: append([]string(nil), module.Files[mark.files:]...),
		Unit:     encoded,
		Interns:  append([]internNote(nil), journal.since(mark.interns)...),
	})
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
	cachefile.Trim(c.directory)
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
// The order of the six steps is the order the cold compile produced them in, and
// that is the point: the file names first, because SrcPos indices resolve against
// them; then the aggregate types, the data and the functions, each appended in the
// order the declaration appended them, so the module's three tables come out
// index for index the same as a cold compile's.
func (c *functionCache) replay(g *gen, declaration *cachedDeclaration) error {
	started := time.Now()
	defer func() { c.stats.Replay += time.Since(started) }()
	decoded, err := ir.DecodeModuleUnverified(declaration.Unit)
	if err != nil {
		return fmt.Errorf("goc: cached unit for %s: %w", declaration.Symbol, err)
	}
	module := g.mod

	// 1. The file names this declaration appended, in order, so the module's file
	// table grows exactly as it did cold.
	for _, name := range declaration.NewFiles {
		module.File(name)
	}
	// 2. The remap from the unit's private table to the module's.
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

	// 3. Intern the aggregate types structurally, then append the ones this
	// declaration declared.
	c.seedAggregates(module)
	canonical := make(map[string]*ir.AggType, len(decoded.Types))
	for _, function := range decoded.Funcs {
		c.internFunctionAggregates(function)
	}
	for _, aggregate := range decoded.Types {
		interned := c.internAggregate(aggregate)
		canonical[interned.Name] = interned
		if !c.declaredAggregates[interned] {
			c.declaredAggregates[interned] = true
			module.AddType(interned)
			c.aggregatesScanned = len(module.Types)
		}
	}

	// 4. The data, with the collision check.
	c.seedData(module)
	for _, datum := range decoded.Data {
		existing := c.data[datum.Name]
		if existing == nil {
			c.data[datum.Name] = datum
			module.Data = append(module.Data, datum)
			continue
		}
		if err := sameDataDefinition(existing, datum); err != nil {
			return fmt.Errorf("goc: cached unit for %s redefines %s: %w", declaration.Symbol, datum.Name, err)
		}
	}

	// 5. The functions.
	for _, function := range decoded.Funcs {
		module.Adopt(function)
		c.stats.Functions++
		for _, block := range function.Blocks {
			c.stats.Instructions += len(block.Instrs)
		}
	}

	// 6. The intern tables, so the next declaration that wants the same literal
	// finds the symbol this one already put in the module.
	for _, note := range declaration.Interns {
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
			// The descriptor is either in this delta -- appended above -- or in one an
			// earlier declaration contributed, so the module is what is searched
			// rather than the unit.
			setRuntimeTypeEqualDescriptor(module, note.Key, note.Value)
		}
	}
	c.replayed[declaration.Decl] = true
	return nil
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
		keys[key] = runtimeTypeKey(fset, types.NewPointer(value))
	}
	for key, pointerKey := range c.pointerKeys {
		keys[key] = pointerKey
	}
	return keys
}

// report is what this compile did, for a driver or a test to read back.
func (c *functionCache) report() FunctionCacheStats { return c.stats }
