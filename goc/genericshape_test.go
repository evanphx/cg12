package goc

import (
	"encoding/json"
	"flag"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/opt"
	"github.com/stretchr/testify/require"
)

// genericShapeCensusOut turns the census on. It is a flag rather than a plain
// test because a census run compiles and optimises a whole program -- minutes
// for the http one -- and because it is measurement, not verification: nothing
// about it should run in verify-fast.
var genericShapeCensusOut = flag.String(
	"generic-shape-census",
	"",
	"write a GC-shape census of the named program's instantiations to this `directory`",
)

var genericShapeCensusProgram = flag.String(
	"generic-shape-census-program",
	"",
	"the goc/testdata program to census; required with -generic-shape-census",
)

// TestGCShapeName pins the shape model against gc's rule. It is cheap and runs
// by default; the census below does not.
func TestGCShapeName(t *testing.T) {
	t.Parallel()

	pkg := types.NewPackage("main", "main")
	field := func(name string, fieldType types.Type) *types.Var {
		return types.NewField(0, pkg, name, fieldType, false)
	}
	defined := map[string]types.Type{}
	define := func(name string, underlying types.Type) {
		object := types.NewTypeName(0, pkg, name, nil)
		defined[name] = types.NewNamed(object, underlying, nil)
	}
	define("myThing", types.NewStruct([]*types.Var{field("n", types.Typ[types.Int])}, nil))
	define("otherThing", types.NewStruct([]*types.Var{field("n", types.Typ[types.Int])}, nil))
	define("namedInt", types.Typ[types.Int])
	define("pair", types.NewStruct([]*types.Var{
		field("a", types.Typ[types.Int]),
		field("b", types.Typ[types.String]),
	}, nil))

	lookup := func(name string) types.Type {
		valueType, ok := defined[name]
		require.True(t, ok, "type %s", name)
		return valueType
	}
	pointerTo := func(name string) types.Type { return types.NewPointer(lookup(name)) }

	// A basic constraint collapses every pointer onto one shape.
	require.Equal(t, "go.shape.*uint8", gcShapeName(pointerTo("myThing"), true))
	require.Equal(t, "go.shape.*uint8", gcShapeName(pointerTo("otherThing"), true))
	require.Equal(t, "go.shape.*uint8", gcShapeName(types.NewPointer(types.Typ[types.Int]), true))

	// A non-basic constraint (comparable, cmp.Ordered, ~[]E) does not.
	require.Equal(t, "go.shape.*main.myThing", gcShapeName(pointerTo("myThing"), false))
	require.NotEqual(t, gcShapeName(pointerTo("myThing"), false), gcShapeName(pointerTo("otherThing"), false))

	// A defined type loses its name; two structs of the same layout share.
	require.Equal(t, gcShapeName(lookup("myThing"), true), gcShapeName(lookup("otherThing"), true))
	require.Equal(t, "go.shape.int", gcShapeName(lookup("namedInt"), true))
	require.NotEqual(t, gcShapeName(lookup("myThing"), true), gcShapeName(lookup("pair"), true))

	// Shaping is shallow: gc strips the top-level name only, so two slices of
	// two distinct element types stay two shapes. Modelling this faithfully is
	// what keeps the census from overstating the collapse.
	require.NotEqual(t,
		gcShapeName(types.NewSlice(lookup("namedInt")), true),
		gcShapeName(types.NewSlice(types.Typ[types.Int]), true),
	)

	// Only *types.Pointer collapses. Maps and channels are one word wide but
	// are not TPTR in gc, so shapify leaves them alone.
	require.NotEqual(t,
		gcShapeName(types.NewMap(types.Typ[types.String], types.Typ[types.Int]), true),
		gcShapeName(types.NewMap(types.Typ[types.String], types.Typ[types.Bool]), true),
	)
	require.NotEqual(t,
		gcShapeName(types.NewChan(types.SendRecv, types.Typ[types.Int]), true),
		gcShapeName(pointerTo("myThing"), true),
	)
}

// TestGenericShapeCensus is the Stage A measurement. Skipped unless
// -generic-shape-census names an output directory.
func TestGenericShapeCensus(t *testing.T) {
	if *genericShapeCensusOut == "" {
		t.Skip("measurement only; pass -generic-shape-census=<dir> -generic-shape-census-program=<file>")
	}
	program := *genericShapeCensusProgram
	require.NotEmpty(t, program, "-generic-shape-census-program is required")

	path := program
	if !strings.Contains(program, string(filepath.Separator)) {
		path = filepath.Join("testdata", program)
	}
	program = filepath.Base(program)
	source, err := os.ReadFile(path)
	require.NoError(t, err)

	finish := installGenericCensus()
	module, err := CompileExecutableFor(HostTarget(), program, source)
	census := finish()
	require.NoError(t, err)

	report := struct {
		Program string `json:"program"`
		*GenericInstantiationCensus
		// LoweredBeforeOpt is every function the module carried out of the
		// front end, before dead-function elimination.
		LoweredBeforeOpt []string `json:"loweredBeforeOpt"`
		// LoweredAfterOpt is what survived the whole pipeline: the functions
		// that are actually optimised to convergence and emitted.
		LoweredAfterOpt []string `json:"loweredAfterOpt"`
		// OptNanos and OptVisits are per-function optimiser accounting, present
		// only when GOC_OPT_FUNCTIME=1.
		OptNanos  map[string]int64 `json:"optNanos,omitempty"`
		OptVisits map[string]int64 `json:"optVisits,omitempty"`
		// OptTotalNanos is the wall time of the whole pipeline, so the
		// per-function figures can be read as a share of something.
		OptTotalNanos int64 `json:"optTotalNanos"`
		// Closure and GenericPackages are Stage 1's package-granular
		// classification, carried here so the two accountings -- "functions in a
		// package that declares a generic" and "functions that are an
		// instantiation" -- can be compared on the same program.
		Closure         []string `json:"closure"`
		GenericPackages []string `json:"genericPackages"`
		// FunctionCache is Stage 2's accounting on the same denominator: what a
		// cache whose unit is a FUNCTION may hold, with the instantiations
		// excluded but their packages kept.
		FunctionCache FunctionCacheCensus `json:"functionCache"`
	}{
		Program:                    program,
		GenericInstantiationCensus: census,
	}
	report.LoweredBeforeOpt = moduleFunctionNames(module)

	eligibility, err := ClassifyPackageCacheEligibility(HostTarget(), program, source)
	require.NoError(t, err)
	report.Closure = eligibility.Closure
	report.GenericPackages = eligibility.Generic
	// Before the optimiser, so the denominator is the same "lowered functions"
	// the instantiation share above is measured against.
	report.FunctionCache = CensusFunctionCache(module, append(append([]string(nil), eligibility.Closure...), census.RootPackage))

	start := time.Now()
	opt.OptimizeModule(module)
	report.OptTotalNanos = int64(time.Since(start))

	report.LoweredAfterOpt = moduleFunctionNames(module)
	report.OptNanos, report.OptVisits = opt.FuncTiming()

	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	out := filepath.Join(*genericShapeCensusOut, strings.TrimSuffix(program, ".go")+".census.json")
	require.NoError(t, os.WriteFile(out, encoded, 0o644))
	t.Logf("census: %d instantiations, %d shapes, %d reachable, %d lowered after opt -> %s",
		len(census.Instantiations), census.ShapeCount(), census.Reachable, len(report.LoweredAfterOpt), out)
	t.Logf("function-granular cache: %d of %d lowered functions cacheable (%.2f%%); excluded %d instantiations, %d interface-call wrappers, %d interface-method dispatchers",
		report.FunctionCache.Cacheable, report.FunctionCache.Lowered, 100*report.FunctionCache.CacheableShare(),
		report.FunctionCache.Instantiations, report.FunctionCache.InterfaceCallWrappers,
		report.FunctionCache.InterfaceMethodDispatchers)
	t.Logf("by IR instruction: %d of %d cacheable (%.2f%%); %d in instantiations, %d in call wrappers, %d in dispatchers",
		report.FunctionCache.CacheableInstructions, report.FunctionCache.LoweredInstructions,
		100*report.FunctionCache.CacheableInstructionShare(),
		report.FunctionCache.InstantiationInstructions, report.FunctionCache.InterfaceCallWrapperInstructions,
		report.FunctionCache.InterfaceMethodDispatcherInstructions)
}

func moduleFunctionNames(module *ir.Module) []string {
	names := make([]string, 0, len(module.Funcs))
	for _, function := range module.Funcs {
		if function.Start == nil {
			continue
		}
		names = append(names, function.Name)
	}
	return names
}
