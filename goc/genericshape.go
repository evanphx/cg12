package goc

import (
	"go/types"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// This file is measurement, not compilation. Nothing in the compile path reads
// a shape; the one hook compile() calls (recordGenericInstantiations) does
// nothing at all unless a caller has installed a sink with
// CensusGenericInstantiations. It exists to answer one question with numbers
// rather than intuition: how many of the instantiations goc monomorphises today
// would collapse onto a shared body under gc's GC shape stenciling.
//
// # The rule being modelled
//
// gc's rule is cmd/compile/internal/noder.shapify (go1.26.1
// src/cmd/compile/internal/noder/reader.go:895). Reduced to what applies to a
// fully-instantiated type argument:
//
//   - If the type parameter's constraint is a *basic interface* -- one whose
//     type set is described by its methods alone, types2.Interface.IsMethodSet --
//     and the argument is a pointer whose element is not go:notinheap, the shape
//     is `*uint8`. This is the collapse that makes every pointer instantiation
//     share one body.
//   - Otherwise the shape is the argument's *underlying* type. A defined type
//     loses its name and its methods; a `type myThing struct{n int}` shapes to
//     `struct{n int}` and shares a body with every other struct of that layout.
//
// and the shape's name is "go.shape." + the underlying type's LinkString.
//
// Two things gc does NOT do, which bound the prize and which this model must
// therefore not do either:
//
//   - Shaping is shallow. `[]MyInt` and `[]YourInt` are distinct shapes, because
//     the underlying type of a slice is the slice itself and only the top-level
//     name is stripped. gc's own TODO in shapify says as much: "It should be
//     possible to do much more aggressive shaping still; e.g., collapsing all
//     pointer-shaped types into a common type, ... recursively shaping the
//     element types of composite types".
//   - Only *types.Pointer collapses. Maps, channels, funcs and unsafe.Pointer
//     are pointer-shaped in memory but are not TPTR, so shapify leaves them
//     alone and `map[string]int` and `map[string]bool` stay two shapes.
//
// Modelling the real rule rather than an idealised one is the point. An
// idealised "everything one pointer wide collapses" model would overstate the
// answer, and the answer is what decides whether the rest of this work happens.

// gcShapePkg is the package gc puts shape types in. Reproduced here so the
// census's shape names read the way a gc symbol does.
const gcShapePkg = "go.shape."

// typeIdentityString is the go/types stand-in for gc's Type.LinkString: a
// string that is equal for two types exactly when they are identical, with
// named types qualified by import path.
func typeIdentityString(valueType types.Type) string {
	return types.TypeString(valueType, func(pkg *types.Package) string { return pkg.Path() })
}

// gcShapeType returns the shape type gc would use for one type argument, given
// whether the type parameter it instantiates is constrained by a basic
// interface. See the file comment for the rule.
func gcShapeType(argument types.Type, basicConstraint bool) types.Type {
	argument = canonicalAliasType(argument)
	underlying := argument.Underlying()
	if basicConstraint {
		if pointer, ok := underlying.(*types.Pointer); ok && !isNotInHeapType(pointer.Elem()) {
			return types.NewPointer(types.Typ[types.Uint8])
		}
	}
	return underlying
}

// gcShapeName is gcShapeType rendered the way a gc shape symbol reads. It is
// for display and for the unit test; shape *identity* goes through
// [shapeInterner], because two distinct types can render the same string.
func gcShapeName(argument types.Type, basicConstraint bool) string {
	return gcShapePkg + typeIdentityString(gcShapeType(argument, basicConstraint))
}

// shapeInterner assigns each distinct shape a stable name.
//
// It cannot key on the rendered string. go/types prints an unexported struct
// field as `struct{x int}` whatever package declared it, but two such structs
// from two packages are not identical types and gc's LinkString -- which
// qualifies the field, `struct { main.x int }` -- keeps them apart. Keying on
// the string would merge two shapes gc separates and overstate the collapse,
// which is the one error this measurement must not make. So identity is
// types.Identical, the language's own relation, and the string is only a label:
// when two non-identical shapes render alike the second gets a `#n` suffix.
type shapeInterner struct {
	representatives []types.Type
	names           []string
	used            map[string]int
}

func (s *shapeInterner) name(shape types.Type) string {
	for index, representative := range s.representatives {
		if types.Identical(representative, shape) {
			return s.names[index]
		}
	}
	if s.used == nil {
		s.used = make(map[string]int)
	}
	name := gcShapePkg + typeIdentityString(shape)
	s.used[name]++
	if count := s.used[name]; count > 1 {
		name += "#" + strconv.Itoa(count)
	}
	s.representatives = append(s.representatives, shape)
	s.names = append(s.names, name)
	return name
}

// aggressiveShapeType is gc's shape rule with gc's own TODO carried out: every
// pointer-shaped word collapses whatever the constraint, composite element
// types are shaped recursively, and a struct keeps its layout but loses its
// field names.
//
//	// TODO(mdempsky): It should be possible to do much more aggressive
//	// shaping still; e.g., collapsing all pointer-shaped types into a
//	// common type, collapsing scalars of the same size/alignment into a
//	// common type, recursively shaping the element types of composite
//	// types, and discarding struct field names and tags.
//
// It exists so the measurement can separate two very different conclusions:
// "stenciling buys nothing here" and "gc's *particular*, deliberately shallow
// stenciling buys nothing here, but a better rule would". Scalars are left
// alone, because collapsing int with uint changes what the arithmetic in the
// shared body means; [layoutShapeKey] is the bound that ignores even that.
func aggressiveShapeType(argument types.Type, depth int) types.Type {
	if depth > 12 {
		return argument.Underlying()
	}
	underlying := canonicalAliasType(argument).Underlying()
	switch value := underlying.(type) {
	case *types.Pointer:
		if isNotInHeapType(value.Elem()) {
			return underlying
		}
		return types.NewPointer(types.Typ[types.Uint8])
	case *types.Map, *types.Chan, *types.Signature:
		// One word, scanned as a pointer. A shared body that needs the element
		// type reads it out of the dictionary, exactly as it would for *T.
		return types.NewPointer(types.Typ[types.Uint8])
	case *types.Slice:
		return types.NewSlice(aggressiveShapeType(value.Elem(), depth+1))
	case *types.Array:
		return types.NewArray(aggressiveShapeType(value.Elem(), depth+1), value.Len())
	case *types.Struct:
		fields := make([]*types.Var, value.NumFields())
		for index := range fields {
			field := value.Field(index)
			fields[index] = types.NewField(0, nil, "_"+strconv.Itoa(index),
				aggressiveShapeType(field.Type(), depth+1), false)
		}
		return types.NewStruct(fields, nil)
	}
	return underlying
}

// layoutShapeKey is the loosest shape a stencil could possibly use: two types
// share it when they have the same size, the same alignment and the same
// pointer/scalar map. This is the definition of "GC shape" in the plain sense
// -- what the garbage collector and the frame layout can tell apart -- and it
// is strictly coarser than anything gc implements. It is reported as an upper
// bound on the prize, not as a proposal: a body shared across every type with
// this key cannot do arithmetic that depends on signedness or float-ness, so
// something would have to constrain which instantiations may use it.
// It returns an "unsized:" key for a type argument that is still, or contains,
// a type parameter. Those exist: the whole-program walk reaches a generic call
// inside a generic body where the outer substitution did not resolve every
// argument, and go/types' Sizeof panics on such a type by design ("Type
// parameters lead to variable sizes/alignments", go/types/gcsizes.go:155). A
// type parameter has no layout, so it has no layout shape either; the fallback
// keeps it distinct rather than silently merging it with something sized.
func layoutShapeKey(argument types.Type, sizes types.Sizes) (key string) {
	argument = canonicalAliasType(argument)
	defer func() {
		if recover() != nil {
			key = "unsized:" + typeIdentityString(argument)
		}
	}()
	var offsets []int64
	collectPointerOffsets(argument, 0, sizes, &offsets, 0)
	var built strings.Builder
	built.WriteString("size=")
	built.WriteString(strconv.FormatInt(sizes.Sizeof(argument), 10))
	built.WriteString(",align=")
	built.WriteString(strconv.FormatInt(sizes.Alignof(argument), 10))
	built.WriteString(",ptr=")
	for index, offset := range offsets {
		if index > 0 {
			built.WriteByte(' ')
		}
		built.WriteString(strconv.FormatInt(offset, 10))
	}
	return built.String()
}

// collectPointerOffsets appends the byte offset of every pointer-bearing word
// of valueType, relative to base. It is the pointer/scalar map the GC sees.
func collectPointerOffsets(valueType types.Type, base int64, sizes types.Sizes, out *[]int64, depth int) {
	if depth > 12 || valueType == nil {
		return
	}
	word := sizes.Sizeof(types.Typ[types.Uintptr])
	switch value := canonicalAliasType(valueType).Underlying().(type) {
	case *types.Pointer, *types.Map, *types.Chan, *types.Signature:
		*out = append(*out, base)
	case *types.Basic:
		switch value.Kind() {
		case types.UnsafePointer:
			*out = append(*out, base)
		case types.String:
			*out = append(*out, base) // {data *byte, len int}
		}
	case *types.Interface:
		// Both words of an eface or iface are pointers as far as the GC is
		// concerned: the first to a type descriptor or itab, the second to the
		// value.
		*out = append(*out, base, base+word)
	case *types.Slice:
		*out = append(*out, base) // {data *E, len, cap int}
	case *types.Array:
		if value.Len() <= 0 {
			return
		}
		stride := sizes.Sizeof(value.Elem())
		for index := int64(0); index < value.Len(); index++ {
			collectPointerOffsets(value.Elem(), base+index*stride, sizes, out, depth+1)
		}
	case *types.Struct:
		fields := make([]*types.Var, value.NumFields())
		for index := range fields {
			fields[index] = value.Field(index)
		}
		for index, offset := range sizes.Offsetsof(fields) {
			collectPointerOffsets(fields[index].Type(), base+offset, sizes, out, depth+1)
		}
	}
}

// isNotInHeapType reports a go:notinheap type: one that embeds
// internal/runtime/sys.NotInHeap, directly or through an embedded field. gc
// excludes pointers to these from the `*uint8` collapse because such a pointer
// is not a heap pointer and the GC must not scan it as one.
func isNotInHeapType(valueType types.Type) bool {
	return notInHeapWithDepth(valueType, 0)
}

func notInHeapWithDepth(valueType types.Type, depth int) bool {
	if depth > 8 || valueType == nil {
		return false
	}
	if named, ok := types.Unalias(valueType).(*types.Named); ok {
		object := named.Obj()
		if object != nil && object.Name() == "NotInHeap" && object.Pkg() != nil {
			switch object.Pkg().Path() {
			case "internal/runtime/sys", "runtime/internal/sys":
				return true
			}
		}
	}
	structure, ok := valueType.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for index := 0; index < structure.NumFields(); index++ {
		field := structure.Field(index)
		// The marker is embedded as a blank field in the runtime
		// (`type notInHeap struct{ _ sys.NotInHeap }`), so an unexported or
		// blank field counts as much as a formally embedded one.
		if notInHeapWithDepth(field.Type(), depth+1) {
			return true
		}
	}
	return false
}

// basicConstraints reports, for each type parameter of a generic function in
// declaration order, whether its constraint is a basic interface. This is the
// `basic` argument gc's writer computes as
// tparam.Underlying().(*types2.Interface).IsMethodSet() (go1.26.1
// src/cmd/compile/internal/noder/writer.go:938).
//
// `any` and `fmt.Stringer` are basic. `comparable` and any constraint written
// with type terms -- `cmp.Ordered`, `~[]E` -- are not, and a pointer passed to
// one of those does not collapse.
func basicConstraints(origin *types.Func) []bool {
	signature, ok := origin.Type().(*types.Signature)
	if !ok {
		return nil
	}
	parameters := signatureTypeParameters(signature)
	basics := make([]bool, len(parameters))
	for index, parameter := range parameters {
		constraint, ok := parameter.Underlying().(*types.Interface)
		basics[index] = ok && constraint.IsMethodSet()
	}
	return basics
}

// GenericInstantiation is one instantiation the whole-program walk discovered,
// with the shape gc would have given it alongside the monomorphic symbol goc
// gives it today.
type GenericInstantiation struct {
	// Symbol is the symbol goc lowers the instantiation under -- what
	// genericInstanceSymbol produced, naming the literal type arguments.
	Symbol string
	// Origin is the symbol of the uninstantiated function.
	Origin string
	// OriginPackage is the import path that declares the generic.
	OriginPackage string
	// TypeArguments are the literal type arguments, path-qualified.
	TypeArguments []string
	// Shapes are the gc shape names of those arguments, positionally.
	Shapes []string
	// ShapeSymbol is the symbol gc would emit the shared body under:
	// Origin[shape,...]. Two instantiations with the same ShapeSymbol share one
	// body under stenciling and are two bodies under monomorphisation.
	ShapeSymbol string
	// AggressiveShapeSymbol is the same under [aggressiveShapeType]: gc's rule
	// with gc's own TODO carried out.
	AggressiveShapeSymbol string
	// LayoutShapeSymbol is the same under [layoutShapeKey]: the loosest shape a
	// stencil could use, size and alignment and pointer map only.
	LayoutShapeSymbol string
	// BasicConstraint records, per type parameter, whether its constraint is a
	// basic interface -- the precondition for the pointer collapse.
	BasicConstraint []bool
	// ArgumentPackages is the declaring import path of each type argument's
	// outermost named type, "" when the argument is unnamed or predeclared.
	ArgumentPackages []string
	// RootTypeArgument reports that some type argument is a type declared in
	// the program being compiled rather than anywhere in the stdlib closure.
	// This is the population a per-package cache cannot hold under
	// monomorphisation, because the instantiation is a function of the importer.
	RootTypeArgument bool
	// PointerArgument reports that some type argument is a pointer.
	PointerArgument bool
}

// GenericInstantiationCensus is one program's worth of instantiations.
type GenericInstantiationCensus struct {
	// RootPackage is the import path of the program being compiled.
	RootPackage string
	// Reachable is the number of function declarations the whole-program walk
	// returned, instantiations included: the denominator for "share of lowered
	// functions" before the optimiser's dead-function elimination runs.
	Reachable int
	// Instantiations is every distinct instantiation, sorted by symbol.
	Instantiations []GenericInstantiation
	// UninstantiatedGenericMethods counts reachable methods on generic types
	// that the walk queued without type arguments -- goc already lowers these
	// once, under a symbol with the type arguments stripped, so they are
	// already shared and are not part of the ratio.
	UninstantiatedGenericMethods int
}

// ShapeCount is the number of distinct shaped bodies the instantiations would
// collapse to under gc's rule.
func (c GenericInstantiationCensus) ShapeCount() int {
	return countDistinct(c.Instantiations, func(i GenericInstantiation) string { return i.ShapeSymbol })
}

// AggressiveShapeCount is ShapeCount under [aggressiveShapeType].
func (c GenericInstantiationCensus) AggressiveShapeCount() int {
	return countDistinct(c.Instantiations, func(i GenericInstantiation) string { return i.AggressiveShapeSymbol })
}

// LayoutShapeCount is ShapeCount under [layoutShapeKey].
func (c GenericInstantiationCensus) LayoutShapeCount() int {
	return countDistinct(c.Instantiations, func(i GenericInstantiation) string { return i.LayoutShapeSymbol })
}

func countDistinct(instantiations []GenericInstantiation, key func(GenericInstantiation) string) int {
	seen := make(map[string]bool, len(instantiations))
	for _, instantiation := range instantiations {
		seen[key(instantiation)] = true
	}
	return len(seen)
}

// genericCensusSink is the installed recorder, nil in every compile that is not
// a census run. compile() checks it and does nothing else.
var (
	genericCensusMu   sync.Mutex
	genericCensusSink *GenericInstantiationCensus
)

// installGenericCensus arms the hook and returns a function that disarms it and
// yields what was recorded.
func installGenericCensus() func() *GenericInstantiationCensus {
	genericCensusMu.Lock()
	census := &GenericInstantiationCensus{}
	genericCensusSink = census
	genericCensusMu.Unlock()
	return func() *GenericInstantiationCensus {
		genericCensusMu.Lock()
		genericCensusSink = nil
		genericCensusMu.Unlock()
		return census
	}
}

// recordGenericInstantiations is the hook compile() calls once the
// whole-program walk has finished. It is a no-op unless a census is installed.
func recordGenericInstantiations(functions []functionDecl, rootPkg *types.Package, sizes types.Sizes) {
	genericCensusMu.Lock()
	census := genericCensusSink
	genericCensusMu.Unlock()
	if census == nil {
		return
	}
	rootPath := ""
	if rootPkg != nil {
		rootPath = rootPkg.Path()
	}
	census.RootPackage = rootPath
	census.Reachable = len(functions)
	shapes := &shapeInterner{}
	aggressive := &shapeInterner{}
	seen := make(map[string]bool)
	for _, function := range functions {
		object, _ := function.info.Defs[function.decl.Name].(*types.Func)
		if object == nil {
			continue
		}
		origin := object.Origin()
		if len(function.typeArguments) == 0 {
			if isGenericFunctionObject(origin) {
				census.UninstantiatedGenericMethods++
			}
			continue
		}
		symbol := function.symbol
		if symbol == "" {
			symbol = genericInstanceSymbol(origin, function.typeArguments)
		}
		if seen[symbol] {
			continue
		}
		seen[symbol] = true
		census.Instantiations = append(census.Instantiations,
			describeInstantiation(origin, function.typeArguments, symbol, rootPath, shapes, aggressive, sizes))
	}
	sort.Slice(census.Instantiations, func(i, j int) bool {
		return census.Instantiations[i].Symbol < census.Instantiations[j].Symbol
	})
}

// isGenericFunctionObject reports a function that is generic in itself or in
// its receiver.
func isGenericFunctionObject(origin *types.Func) bool {
	signature, ok := origin.Type().(*types.Signature)
	if !ok {
		return false
	}
	return signature.TypeParams().Len() > 0 || signature.RecvTypeParams().Len() > 0
}

func describeInstantiation(origin *types.Func, arguments []types.Type, symbol, rootPath string, shapes, aggressive *shapeInterner, sizes types.Sizes) GenericInstantiation {
	basics := basicConstraints(origin)
	instantiation := GenericInstantiation{
		Symbol:          symbol,
		Origin:          functionSymbol(origin),
		BasicConstraint: basics,
	}
	if origin.Pkg() != nil {
		instantiation.OriginPackage = origin.Pkg().Path()
	}
	var shaped, shapedAggressive, shapedLayout strings.Builder
	shaped.WriteString(instantiation.Origin)
	shapedAggressive.WriteString(instantiation.Origin)
	shapedLayout.WriteString(instantiation.Origin)
	shaped.WriteByte('[')
	shapedAggressive.WriteByte('[')
	shapedLayout.WriteByte('[')
	for index, argument := range arguments {
		argument = canonicalAliasType(argument)
		basic := index < len(basics) && basics[index]
		shape := shapes.name(gcShapeType(argument, basic))
		if index > 0 {
			shaped.WriteByte(',')
			shapedAggressive.WriteByte(',')
			shapedLayout.WriteByte(',')
		}
		shaped.WriteString(shape)
		shapedAggressive.WriteString(aggressive.name(aggressiveShapeType(argument, 0)))
		shapedLayout.WriteString(layoutShapeKey(argument, sizes))
		instantiation.TypeArguments = append(instantiation.TypeArguments, typeIdentityString(argument))
		instantiation.Shapes = append(instantiation.Shapes, shape)
		path := typeArgumentPackage(argument)
		instantiation.ArgumentPackages = append(instantiation.ArgumentPackages, path)
		if path != "" && path == rootPath {
			instantiation.RootTypeArgument = true
		}
		if _, isPointer := argument.Underlying().(*types.Pointer); isPointer {
			instantiation.PointerArgument = true
		}
	}
	shaped.WriteByte(']')
	shapedAggressive.WriteByte(']')
	shapedLayout.WriteByte(']')
	instantiation.ShapeSymbol = shaped.String()
	instantiation.AggressiveShapeSymbol = shapedAggressive.String()
	instantiation.LayoutShapeSymbol = shapedLayout.String()
	return instantiation
}

// typeArgumentPackage is the import path a type argument's outermost named type
// was declared in. For an unnamed composite it descends into the element,
// because `[]main.myThing` is as much a program-local type as `main.myThing` is
// for the purpose of "could another program have asked for this".
func typeArgumentPackage(argument types.Type) string {
	return typeArgumentPackageDepth(argument, 0)
}

func typeArgumentPackageDepth(argument types.Type, depth int) string {
	if depth > 8 || argument == nil {
		return ""
	}
	switch value := types.Unalias(argument).(type) {
	case *types.Named:
		if value.Obj() != nil && value.Obj().Pkg() != nil {
			return value.Obj().Pkg().Path()
		}
		return ""
	case *types.Pointer:
		return typeArgumentPackageDepth(value.Elem(), depth+1)
	case *types.Slice:
		return typeArgumentPackageDepth(value.Elem(), depth+1)
	case *types.Array:
		return typeArgumentPackageDepth(value.Elem(), depth+1)
	case *types.Chan:
		return typeArgumentPackageDepth(value.Elem(), depth+1)
	case *types.Map:
		if path := typeArgumentPackageDepth(value.Key(), depth+1); path != "" {
			return path
		}
		return typeArgumentPackageDepth(value.Elem(), depth+1)
	}
	return ""
}
