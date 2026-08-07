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
// collapse to.
func (c GenericInstantiationCensus) ShapeCount() int {
	shapes := make(map[string]bool, len(c.Instantiations))
	for _, instantiation := range c.Instantiations {
		shapes[instantiation.ShapeSymbol] = true
	}
	return len(shapes)
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
func recordGenericInstantiations(functions []functionDecl, rootPkg *types.Package) {
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
			describeInstantiation(origin, function.typeArguments, symbol, rootPath, shapes))
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

func describeInstantiation(origin *types.Func, arguments []types.Type, symbol, rootPath string, shapes *shapeInterner) GenericInstantiation {
	basics := basicConstraints(origin)
	instantiation := GenericInstantiation{
		Symbol:          symbol,
		Origin:          functionSymbol(origin),
		BasicConstraint: basics,
	}
	if origin.Pkg() != nil {
		instantiation.OriginPackage = origin.Pkg().Path()
	}
	var shaped strings.Builder
	shaped.WriteString(instantiation.Origin)
	shaped.WriteByte('[')
	for index, argument := range arguments {
		argument = canonicalAliasType(argument)
		basic := index < len(basics) && basics[index]
		shape := shapes.name(gcShapeType(argument, basic))
		if index > 0 {
			shaped.WriteByte(',')
		}
		shaped.WriteString(shape)
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
	instantiation.ShapeSymbol = shaped.String()
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
