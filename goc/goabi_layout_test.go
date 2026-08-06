package goc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"runtime"
	"testing"

	"github.com/evanphx/cg12/ir"
)

// A Go type is described to the collector twice, by two pieces of code that
// share nothing.
//
// The heap description is the abi.Type's GCData mask, built by walkPointerWords
// from go/types offsets. The frame description is the Go-ABI aggregate
// (ir.AggType) built by goABIAggregate, from which ir.AggregatePointerOffsets
// produces the pointer words that MarkAggregatePointerWords puts in the frame's
// stack map, and whose Layout sizes the frame slot the value lives in.
//
// They must say the same thing. When they disagreed -- a zero-length array
// field emitted as one whole element, because ir.Field.Count is 0 for an
// ordinary scalar field too and ir.Field.count() reads that as one -- the frame
// description gained a pointer word over log/slog's packed uint64 and shifted
// every later field of slog.Attr by a word, while the heap description stayed
// correct. The programs died in the collector and in the stack copier; see
// goc/testdata/slog_attr_frame_gcmask.go and RUNTIME_PLAN.md section 28.
//
// This is the invariant that says so directly, so a future divergence is a unit
// test failure and not a fatal error in a program that happens to hold the
// wrong kind of value in a frame.
func requireAggregateMatchesType(t *testing.T, g *gen, name string, valueType types.Type) {
	t.Helper()
	aggregate := g.goABIAggregate(valueType)
	if aggregate == nil {
		// Not an aggregate for ABI purposes -- a scalar, a pointer, a map, a
		// channel or a func value. Those carry no aggregate to disagree with.
		return
	}

	size, alignment := aggregate.Layout()
	if int64(size) != typeSize(valueType) {
		t.Errorf("%s: aggregate lays out as %d bytes, the type is %d", name, size, typeSize(valueType))
	}
	if int64(alignment) != typeAlign(valueType) {
		t.Errorf("%s: aggregate aligns to %d, the type to %d", name, alignment, typeAlign(valueType))
	}

	want := make([]int, 0, 4)
	for _, word := range pointerWordIndices(valueType) {
		want = append(want, word*8)
	}
	got := ir.AggregatePointerOffsets(aggregate)
	if len(got) != len(want) {
		t.Errorf("%s: aggregate pointer offsets %v, the type descriptor's are %v", name, got, want)
		return
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("%s: aggregate pointer offsets %v, the type descriptor's are %v", name, got, want)
			return
		}
	}
}

// aggregateProbeGen returns a gen holding only what goABIAggregate reads: the
// file set its cache keys are built from, a module to register aggregates in,
// the cache itself, and the flag that says this compilation has a managed
// runtime and therefore Go-ABI aggregates at all.
func aggregateProbeGen(fset *token.FileSet) *gen {
	return &gen{
		fset:              fset,
		mod:               ir.NewModule(),
		runtimeAllocation: true,
		goABITypes:        map[string]*ir.AggType{},
	}
}

// TestGoABIAggregateAgreesWithTheTypeLayout checks the invariant on shapes
// chosen to put a zero-sized field everywhere it can go -- first, last, in the
// middle, alone, nested, and as an array of a pointer-shaped element -- along
// with the ordinary shapes that must not move.
func TestGoABIAggregateAgreesWithTheTypeLayout(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "layout.go", `package layout

// log/slog's Value and Attr, which is where this mattered.
type Value struct {
	_   [0]func()
	num uint64
	any any
}

type Attr struct {
	Key   string
	Value Value
}

// A zero-length array of a pointer-shaped element in every position.
type Leading struct {
	_ [0]func()
	n uint64
	p *int
}

type Middle struct {
	p *int
	_ [0]func()
	n uint64
}

type Trailing struct {
	p *int
	n uint64
	_ [0]func()
}

type Alone struct {
	_ [0]func()
}

// The other zero-sized forms: an empty struct, an empty array of a struct, and
// an empty array of a scalar.
type TrailingEmptyStruct struct {
	n uint64
	_ struct{}
}

type LeadingEmptyStruct struct {
	_ struct{}
	p *int
}

type EmptyArrayOfStruct struct {
	_ [0]struct{ p *int }
	n uint64
}

type EmptyArrayOfScalar struct {
	n uint64
	_ [0]byte
}

// Ordinary shapes, which must be unchanged.
type Ordinary struct {
	items  [3]*int
	slice  []byte
	text   string
	boxed  any
	scalar float64
	inner  Value
}

type Words [4]uint64

type Pointers [2]*int

type Empty [0]*int

type Mixed struct {
	flag  bool
	count int32
	tail  *Mixed
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	configuration := types.Config{Sizes: types.SizesFor("gc", runtime.GOARCH)}
	pkg, err := configuration.Check("layout", fset, []*ast.File{file}, nil)
	if err != nil {
		t.Fatal(err)
	}

	g := aggregateProbeGen(fset)
	names := pkg.Scope().Names()
	if len(names) == 0 {
		t.Fatal("no types were declared")
	}
	for _, name := range names {
		typeName, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		requireAggregateMatchesType(t, g, name, typeName.Type())
	}
}

// TestGoABIAggregatesAgreeWithTheirTypesInTheStdlib runs the same invariant
// over every named type of the standard-library packages that actually declare
// a zero-length array field, using the source the compiler compiles rather than
// a copy of the shapes.
//
//	log/slog     Value    _ [0]func()   the type the defect was found on
//	sync/atomic  Pointer  _ [0]*T
//	weak         Pointer  _ [0]*T
//	runtime      PanicNilError  _ [0]*PanicNilError
//
// Every one of those elements is pointer-shaped, so every one of them used to
// claim a pointer word over whatever followed it. slog.Value is the one where
// what followed it was a uint64 holding a number, which is why that is the one
// that killed programs.
func TestGoABIAggregatesAgreeWithTheirTypesInTheStdlib(t *testing.T) {
	t.Parallel()
	packages := []string{"log/slog", "sync/atomic", "weak", "runtime"}
	for _, path := range packages {
		t.Run(path, func(t *testing.T) {
			fset := token.NewFileSet()
			loader := newSourceLoader(fset, HostTarget())
			pkg, err := loader.Import(path)
			if err != nil {
				t.Skipf("%s source unavailable: %v", path, err)
			}

			g := aggregateProbeGen(fset)
			checked := 0
			for _, name := range pkg.Scope().Names() {
				typeName, ok := pkg.Scope().Lookup(name).(*types.TypeName)
				if !ok {
					continue
				}
				requireAggregateMatchesType(t, g, path+"."+name, typeName.Type())
				checked++
			}
			if checked == 0 {
				t.Fatalf("%s declared no types, so nothing was checked", path)
			}
			if path == "log/slog" && pkg.Scope().Lookup("Value") == nil {
				t.Fatal("log/slog.Value was not found, so the type this test exists for was not checked")
			}
		})
	}
}
