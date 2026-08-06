package goc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/evanphx/cg12/ir"
	"github.com/stretchr/testify/require"
)

// This file is the deliverable of "lowering is a function of the package
// alone": it compiles two different programs that import the same packages and
// compares, function by function, the IR those shared packages lowered to.
//
// Why it has to be a comparison of two whole programs rather than an assertion
// about one. Every mechanism that made lowering program-dependent was invisible
// from inside a single compile -- devirtualisation consulted the whole
// program's dynamic types and got a self-consistent answer, the initializer
// symbol numbered itself against the whole program's initializer list and got a
// unique name. Each is only wrong relative to another program, so the test has
// to hold two programs at once.
//
// The digest is the encoded unit, not the printed IL, because the printed form
// omits fields that reach the image (a datum's relocation base, an aggregate
// parameter's type, the call convention). Source-file indices are renumbered
// per function into first-use order before encoding, because SrcPos.File is an
// index into the module's file table and the table is interned in lowering
// order -- which is a property of the program, not of the package. The file
// *names* still take part in the digest, so a genuine position change is still
// caught.

// programSmall imports strconv and nothing else, so the whole-program facts a
// compile of it can see are as thin as they get.
const programSmall = `package main

import "strconv"

func main() {
	println(strconv.Itoa(41))
	q, err := strconv.ParseFloat("1.5", 64)
	if err != nil {
		println("unparsed")
	}
	println(int(q))
	println(strconv.Quote("hi"))
}
`

// programWide imports the same package from a program with many more types
// implementing many more interfaces: three concrete error implementations, a
// fmt.Stringer, a sort.Interface, and everything fmt drags in. The point is
// that collectDynamicTypes and reachableFunctions answer very differently here
// than they do for programSmall, so anything in lowering that consults them
// produces different IR for the packages the two programs share.
const programWide = `package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type boxed struct{ n int }

func (b boxed) Error() string  { return "boxed " + strconv.Itoa(b.n) }
func (b boxed) String() string { return "boxed" }

type wrapped struct{ inner error }

func (w wrapped) Error() string { return "wrapped: " + w.inner.Error() }
func (w wrapped) Unwrap() error { return w.inner }

type counted int

func (c counted) Error() string { return strconv.Itoa(int(c)) }

type ordered []int

func (o ordered) Len() int           { return len(o) }
func (o ordered) Less(i, j int) bool { return o[i] < o[j] }
func (o ordered) Swap(i, j int)      { o[i], o[j] = o[j], o[i] }

func main() {
	var err error = wrapped{inner: boxed{3}}
	fmt.Println(err, errors.Is(err, err), errors.Unwrap(err))
	var other error = counted(7)
	fmt.Println(other)

	o := ordered{3, 1, 2}
	sort.Sort(o)
	fmt.Println(o, strconv.Itoa(41))

	q, _ := strconv.ParseFloat("1.5", 64)
	fmt.Println(q, strconv.Quote("hi"), strings.ToUpper("x"))

	var sb strings.Builder
	fmt.Fprintf(&sb, "%v %d", err, 12)
	fmt.Println(sb.String())
}
`

// loweredFunctionDigests digests every function of a module under a file table
// renumbered per function. See the file comment for why.
func loweredFunctionDigests(t *testing.T, m *ir.Module) map[string]string {
	t.Helper()
	digests := make(map[string]string, len(m.Funcs))
	for _, f := range m.Funcs {
		if generatedAfterLowering(f) {
			continue
		}
		digest, err := loweredFunctionDigest(f)
		require.NoError(t, err, "digesting %s", f.Name)
		digests[f.Name] = digest
	}
	return digests
}

func loweredFunctionDigest(f *ir.Func) (string, error) {
	owner := f.Module()
	var files []string
	renumbered := make(map[uint32]uint32)
	remap := func(pos *ir.SrcPos) {
		if pos.File == 0 {
			return
		}
		index, seen := renumbered[pos.File]
		if !seen {
			name := ""
			if owner != nil {
				name = owner.FileName(pos.File)
			}
			files = append(files, name)
			index = uint32(len(files))
			renumbered[pos.File] = index
		}
		pos.File = index
	}
	// An InlineSite is shared by every instruction the inliner spliced in from
	// one call, so it is visited many times and must be renumbered once. Without
	// this the second visit renumbers an already-renumbered index and the digest
	// becomes a function of how many instructions the splice produced.
	visited := make(map[*ir.InlineSite]bool)
	for _, block := range f.Blocks {
		remap(&block.Pos)
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			remap(&instruction.Pos)
			for site := instruction.Inl; site != nil && !visited[site]; site = site.Parent {
				visited[site] = true
				remap(&site.Call)
			}
		}
	}

	scratch := &ir.Module{Funcs: []*ir.Func{f}, Files: files}
	encoded, err := scratch.MarshalBinary()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:8]), nil
}

// packageOfFunction attributes a lowered function to the Go package that
// declared the source it came from. goc names a function `<import path>.<rest>`
// and an import path may itself contain dots, so the attribution is by longest
// match against the set of import paths the compile loaded rather than by
// splitting on the first dot.
func packageOfFunction(name string, paths []string) string {
	best := ""
	for _, path := range paths {
		if len(path) > len(best) && strings.HasPrefix(name, path+".") {
			best = path
		}
	}
	return best
}

// loadedImportPaths is every import path either program's closure can contain.
// It is deliberately a fixed list read out of the modules themselves rather
// than out of the loader, so the comparison stays a comparison of two finished
// modules.
func loadedImportPaths(modules ...*ir.Module) []string {
	candidates := make(map[string]bool)
	for _, m := range modules {
		for _, f := range m.Funcs {
			name := f.Name
			if strings.HasPrefix(name, ".goc.") {
				continue
			}
			// Trim from the last dot back, offering every prefix as a candidate
			// path; only those that several functions agree on survive below.
			for index := len(name) - 1; index > 0; index-- {
				if name[index] == '.' {
					candidates[name[:index]] = true
				}
			}
		}
	}
	// A candidate is a real import path only if it is not itself the prefix of a
	// longer candidate ending in a path separator... which is not decidable from
	// names alone. Instead keep candidates that contain no dot after the last
	// slash boundary is irrelevant: use the shortest candidate that is a prefix
	// of a name, which is the import path, because everything after the import
	// path in a goc symbol begins with a dot-separated Go identifier.
	paths := make([]string, 0, len(candidates))
	for candidate := range candidates {
		paths = append(paths, candidate)
	}
	sort.Strings(paths)
	shortest := make([]string, 0, len(paths))
	for _, candidate := range paths {
		covered := false
		for _, kept := range shortest {
			if strings.HasPrefix(candidate, kept+".") {
				covered = true
				break
			}
		}
		if !covered {
			shortest = append(shortest, candidate)
		}
	}
	return shortest
}

// generatedAfterLowering reports that a function was synthesized from the
// assembled module rather than lowered from a package's source, so it is
// program-dependent by construction and a per-package cache would not hold it.
//
// Two families qualify, and both are generated after compile's funcDecl loop
// has finished:
//
//   - An interface-method dispatcher (addInterfaceMethodWrappers) switches over
//     every concrete type in the program that implements the interface. It ends
//     by calling runtime_gocInterfaceDispatchFailure -- or abort, in the
//     freestanding subset -- and halting, and it is synthesized rather than
//     lowered, so it carries no source position at all. Both are required here:
//     runtime.goroutineReady is an ordinary function that also ends in that
//     shape, because a failed type assertion lowers to it, and excluding it
//     would hide exactly the kind of difference this test looks for.
//   - An interface-call wrapper's body is chosen by
//     redirectUnavailableInterfaceCallWrappers, which points it at
//     runtime.unreachableMethod when the program did not lower the method it
//     wraps. interfaceCallWrapperMethodSymbol is the compiler's own predicate
//     for the name.
func generatedAfterLowering(f *ir.Func) bool {
	if _, isCallWrapper := interfaceCallWrapperMethodSymbol(f.Name); isCallWrapper {
		return true
	}
	halts := false
	for _, block := range f.Blocks {
		if block.Pos.Valid() {
			return false
		}
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.Pos.Valid() {
				return false
			}
			if instruction.Op != ir.OCall || len(instruction.Args) == 0 || block.Jmp.Kind != ir.JmpHlt {
				continue
			}
			callee := instruction.Arg(0)
			if callee.Kind != ir.RefConst {
				continue
			}
			constant := f.Consts[callee.ID]
			if constant.Kind == ir.ConstSym &&
				(constant.Sym == "runtime_gocInterfaceDispatchFailure" || constant.Sym == "abort") {
				halts = true
			}
		}
	}
	return halts
}

type packageComparison struct {
	path      string
	common    int
	identical int
	differing []string
	onlyA     int
	onlyB     int
}

func comparePackages(t *testing.T, left, right *ir.Module) map[string]*packageComparison {
	t.Helper()
	leftDigests := loweredFunctionDigests(t, left)
	rightDigests := loweredFunctionDigests(t, right)
	paths := loadedImportPaths(left, right)

	byPackage := make(map[string]*packageComparison)
	comparison := func(path string) *packageComparison {
		existing, ok := byPackage[path]
		if !ok {
			existing = &packageComparison{path: path}
			byPackage[path] = existing
		}
		return existing
	}
	for name, digest := range leftDigests {
		path := packageOfFunction(name, paths)
		if path == "" || path == "main" {
			continue
		}
		entry := comparison(path)
		other, present := rightDigests[name]
		switch {
		case !present:
			entry.onlyA++
		case other == digest:
			entry.common++
			entry.identical++
		default:
			entry.common++
			entry.differing = append(entry.differing, name)
		}
	}
	for name := range rightDigests {
		if _, present := leftDigests[name]; present {
			continue
		}
		path := packageOfFunction(name, paths)
		if path == "" || path == "main" {
			continue
		}
		comparison(path).onlyB++
	}
	for _, entry := range byPackage {
		sort.Strings(entry.differing)
	}
	return byPackage
}

// TestPackageLoweringIsProgramIndependent is the property per-package caching
// needs: the same package, lowered as part of two different programs, produces
// the same IR both times.
//
// It compares every package the two programs share, not one chosen package,
// because the failure this guards against is not confined to a package anyone
// would think to name -- the devirtualisation that made lowering
// program-dependent fired wherever an interface method call appeared in an
// escape question, which is most of the standard library.
//
// Functions present in only one of the two modules are counted and not
// compared. goc lowers the reachable set, so a package contributes different
// functions to different programs by construction; that is a property of the
// compile, not of the lowering, and a per-package cache answers it by lowering
// the whole package rather than by keying on the reachable set.
func TestPackageLoweringIsProgramIndependent(t *testing.T) {
	t.Parallel()

	small, err := CompileExecutableFor(TargetARM64, "small.go", []byte(programSmall))
	require.NoError(t, err)
	wide, err := CompileExecutableFor(TargetARM64, "wide.go", []byte(programWide))
	require.NoError(t, err)

	byPackage := comparePackages(t, small, wide)

	paths := make([]string, 0, len(byPackage))
	for path := range byPackage {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	totalCommon, totalIdentical := 0, 0
	var offenders []string
	for _, path := range paths {
		entry := byPackage[path]
		totalCommon += entry.common
		totalIdentical += entry.identical
		if len(entry.differing) == 0 {
			continue
		}
		sample := entry.differing
		if len(sample) > 6 {
			sample = sample[:6]
		}
		offenders = append(offenders, fmt.Sprintf("  %-32s %4d of %4d shared functions differ: %s",
			path, len(entry.differing), entry.common, strings.Join(sample, ", ")))
	}

	t.Logf("%d packages shared by both programs, %d functions lowered by both, %d identical",
		len(paths), totalCommon, totalIdentical)
	if len(offenders) > 0 {
		t.Errorf("lowering is not a function of the package alone: %d of %d shared functions differ between the two programs\n%s",
			totalCommon-totalIdentical, totalCommon, strings.Join(offenders, "\n"))
	}
}

// TestGlobalInitializerSymbolsAreProgramIndependent names on its own the
// property that the test above only catches through the bodies that call these
// functions: a package's initializer function is called the same thing in every
// program that loads the package.
//
// The symbol used to carry the initializer's rank in a list gathered over every
// loaded unit, so adding one initialized global to a package that sorts earlier
// renamed every later package's initializer -- and renamed the call to it inside
// the initializing code of every other global in the same package.
func TestGlobalInitializerSymbolsAreProgramIndependent(t *testing.T) {
	t.Parallel()

	small, err := CompileExecutableFor(TargetARM64, "small.go", []byte(programSmall))
	require.NoError(t, err)
	wide, err := CompileExecutableFor(TargetARM64, "wide.go", []byte(programWide))
	require.NoError(t, err)

	initializerSymbols := func(m *ir.Module) map[string]bool {
		symbols := make(map[string]bool)
		for _, f := range m.Funcs {
			if strings.HasPrefix(f.Name, ".goc.global.initfunc.") {
				symbols[f.Name] = true
			}
		}
		return symbols
	}
	left, right := initializerSymbols(small), initializerSymbols(wide)
	require.NotEmpty(t, left, "the small program initializes no global dynamically")

	// Both programs load runtime, errors and strconv, so every initializer the
	// small program has must appear under the same name in the wide one. The
	// converse does not hold: the wide program loads more packages.
	var missing []string
	for symbol := range left {
		if !right[symbol] {
			missing = append(missing, symbol)
		}
	}
	sort.Strings(missing)
	require.Emptyf(t, missing,
		"%d of %d initializer symbols in the small program are named differently in the wide one:\n  %s",
		len(missing), len(left), strings.Join(missing, "\n  "))
	t.Logf("%d initializer symbols, all named identically in both programs", len(left))
}
