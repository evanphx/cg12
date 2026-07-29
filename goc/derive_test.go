package goc

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"sort"
	"testing"

	"github.com/evanphx/cg12/ir"
)

// wholeCompilationGenFields lists the gen fields that describe the compilation
// rather than the function being built, and so must survive derive.
//
// This list is the classification from gen's doc comment written down where a
// test can enforce it. Adding a gen field means deciding which side of the line
// it falls on and saying so here; TestDeriveClassifiesEveryGenField fails until
// that happens, which is the check the original target bug did not have. Back
// then target was set on only the outermost of eleven hand-written &gen
// literals, the other ten silently got the zero Target, and the set of failing
// tests did not change at all -- only the architecture name inside the failure
// messages, which went from "unsupported on amd64" to "unsupported on ".
var wholeCompilationGenFields = []string{
	"mod",
	"target",
	"fset",
	"globals",
	"globalLinkNames",
	"methodTargets",
	"functionDecls",
	"noWriteBarrierFunctions",
	"interfaceMethods",
	"interfaceItabs",
	"interfaceCallWrappers",
	"dynamicTypes",
	"reachableGlobals",
	"filterGlobals",
	"emitRuntimeTables",
	"runtimeAllocation",
	"typeTags",
	// The interning maps below are whole-compilation state for the same reason
	// typeTags is: they decide a symbol's name from its content, so a derived
	// generator that started with an empty one would mint a second symbol for
	// something the parent had already emitted.
	"contentSymbols",
	"functionDescriptors",
	"literalData",
	"runtimeTypes",
	"goABITypes",
	"linkNames",
	"initSymbols",
	"dynamicInitializers",
	"dynamicInitializerGuards",
	"dynamicInitializerFunctions",
}

// fullyPopulatedGen returns a gen with every single field set to a distinct
// non-zero value, so that "did derive keep this field?" is answerable field by
// field. The values are meaningless; only their identity matters.
//
// It is a keyed literal rather than a reflective filler because reflect cannot
// assign to unexported fields even from inside the package. That makes it the
// other half of the field-count check: a newly added gen field is zero here, and
// TestDeriveClassifiesEveryGenField reports it as unpopulated.
func fullyPopulatedGen() *gen {
	return &gen{
		// Whole-compilation state.
		mod:                         ir.NewModule(),
		target:                      TargetAMD64,
		fset:                        token.NewFileSet(),
		globals:                     map[types.Object]string{},
		globalLinkNames:             map[types.Object]string{},
		methodTargets:               map[string]*types.Func{},
		functionDecls:               map[*types.Func]functionDecl{},
		noWriteBarrierFunctions:     map[*types.Func]bool{},
		interfaceMethods:            map[*types.Func]bool{},
		interfaceItabs:              map[string]string{},
		interfaceCallWrappers:       map[string]string{},
		dynamicTypes:                []types.Type{types.Typ[types.Int]},
		reachableGlobals:            map[types.Object]bool{},
		filterGlobals:               true,
		emitRuntimeTables:           true,
		runtimeAllocation:           true,
		typeTags:                    map[string]string{},
		contentSymbols:              map[string]string{},
		functionDescriptors:         map[string]string{},
		literalData:                 map[string]string{},
		runtimeTypes:                map[string]types.Type{},
		goABITypes:                  map[string]*ir.AggType{},
		linkNames:                   map[*types.Func]string{},
		initSymbols:                 map[*types.Func]string{},
		dynamicInitializers:         map[types.Object]*globalInitializer{},
		dynamicInitializerGuards:    map[types.Object]string{},
		dynamicInitializerFunctions: map[types.Object]string{},

		// Source context.
		file: &ast.File{},
		info: &types.Info{},
		pkg:  types.NewPackage("example.com/p", "p"),

		// Per-function state.
		fn:                            &ir.Func{},
		cur:                           &ir.Block{},
		seq:                           7,
		err:                           errors.New("sentinel"),
		functionName:                  "sentinel",
		currentFunction:               types.NewFunc(token.NoPos, nil, "f", types.NewSignatureType(nil, nil, nil, nil, nil, false)),
		typeArguments:                 []types.Type{types.Typ[types.Int]},
		noWriteBarrier:                true,
		forceStackVariadic:            true,
		resultSlot:                    ir.Ref{Kind: ir.RefTemp, ID: 1},
		resultType:                    types.Typ[types.Int],
		resultObjects:                 map[types.Object]bool{},
		aggregateResult:               ir.Ref{Kind: ir.RefTemp, ID: 2},
		extraResultSlots:              []ir.Ref{ir.R},
		extraResultTypes:              []types.Type{types.Typ[types.Int]},
		vars:                          map[types.Object]ir.Ref{},
		directValues:                  map[types.Object]bool{},
		stackAddresses:                map[uint32]bool{},
		heapCaptures:                  map[types.Object]ir.Ref{},
		escapingCaptures:              map[types.Object]bool{},
		objectEscapeChecks:            map[types.Object]bool{},
		keepAliveObjects:              map[types.Object]bool{},
		keepAliveValues:               map[types.Object]ir.Ref{},
		keepAliveSlots:                map[types.Object]ir.Ref{},
		transientInterfaceDescriptors: map[uint32]bool{},
		initializingGlobals:           map[types.Object]bool{},
		parents:                       map[ast.Node]ast.Node{},
		currentBody:                   &ast.BlockStmt{},
		breaks:                        []*ir.Block{{}},
		continues:                     []*ir.Block{{}},
		labels:                        map[string]*ir.Block{},
		labeledBreaks:                 map[string]*ir.Block{},
		labeledContinues:              map[string]*ir.Block{},
		deferSlots:                    map[*ast.DeferStmt]ir.Ref{},
		deferFunctions:                map[*ast.DeferStmt]ir.Ref{},
		deferOrder:                    []*ast.DeferStmt{{}},
		deferActions:                  []*ast.DeferStmt{{}},
		deferBlocks:                   []*ir.Block{{}},
		runningDefers:                 true,
		nextCallUsesClosure:           true,
		nextCallClosure:               ir.Ref{Kind: ir.RefTemp, ID: 3},
	}
}

// genFieldIdentity summarizes a gen field well enough to tell "the same value as
// the parent" from "reset". Reference kinds are reduced to their pointer, so an
// inherited map is distinguishable from a freshly allocated empty one, which
// reflect.DeepEqual would call equal.
//
// It reads through unexported fields, which reflect permits; it deliberately
// never calls Value.Interface, which reflect does not.
func genFieldIdentity(value reflect.Value) string {
	switch value.Kind() {
	case reflect.Bool:
		return fmt.Sprintf("bool:%t", value.Bool())
	case reflect.Int:
		return fmt.Sprintf("int:%d", value.Int())
	case reflect.Uint8, reflect.Uint32:
		return fmt.Sprintf("uint:%d", value.Uint())
	case reflect.String:
		return "string:" + value.String()
	case reflect.Map, reflect.Slice, reflect.Pointer:
		if value.IsNil() {
			return "nil"
		}
		return fmt.Sprintf("ref:%#x", value.Pointer())
	case reflect.Interface:
		if value.IsNil() {
			return "nil"
		}
		return genFieldIdentity(value.Elem())
	case reflect.Struct:
		parts := make([]string, value.NumField())
		for index := range parts {
			parts[index] = genFieldIdentity(value.Field(index))
		}
		return "struct{" + fmt.Sprint(parts) + "}"
	default:
		return "unhandled kind " + value.Kind().String()
	}
}

// TestDeriveClassifiesEveryGenField is the guard on gen's whole-compilation
// versus per-generator split: every field is either inherited by derive or reset
// by it, and which one is not left to chance.
func TestDeriveClassifiesEveryGenField(t *testing.T) {
	wholeCompilation := make(map[string]bool, len(wholeCompilationGenFields))
	for _, name := range wholeCompilationGenFields {
		wholeCompilation[name] = true
	}

	parent := fullyPopulatedGen()
	derived := parent.derive()

	parentValue := reflect.ValueOf(parent).Elem()
	derivedValue := reflect.ValueOf(derived).Elem()
	genType := parentValue.Type()

	var unpopulated []string
	for index := 0; index < genType.NumField(); index++ {
		name := genType.Field(index).Name
		parentField := parentValue.Field(index)

		// A field left zero here would compare "reset" no matter what derive
		// does, so the classification below would be vacuous for it.
		if parentField.IsZero() {
			unpopulated = append(unpopulated, name)
			continue
		}

		inherited := genFieldIdentity(parentField) == genFieldIdentity(derivedValue.Field(index))
		switch {
		case wholeCompilation[name] && !inherited:
			t.Errorf("gen.%s is listed as whole-compilation state but derive reset it; "+
				"derived generators must see the same value as their parent", name)
		case !wholeCompilation[name] && inherited:
			t.Errorf("gen.%s is not listed as whole-compilation state but derive inherited it; "+
				"either reset it in derive or add it to wholeCompilationGenFields", name)
		}
	}

	if len(unpopulated) > 0 {
		sort.Strings(unpopulated)
		t.Errorf("fullyPopulatedGen leaves %v zero, so derive's handling of those fields is untested; "+
			"a new gen field has to be given a non-zero value there and classified in "+
			"wholeCompilationGenFields", unpopulated)
	}
}

// TestDeriveKeepsTarget pins the specific field whose omission caused the bug
// derive exists to prevent: a derived generator built for no target at all,
// which turns "unsupported on amd64" into "unsupported on " without changing
// which tests fail.
func TestDeriveKeepsTarget(t *testing.T) {
	for _, target := range []Target{TargetARM64, TargetAMD64} {
		parent := &gen{target: target, mod: ir.NewModule()}
		derived := parent.derive()
		if derived.target != target {
			t.Errorf("derive() target = %q, want %q", derived.target, target)
		}
		if derived.target == "" {
			t.Errorf("derive() produced a generator with no target")
		}
	}
}
