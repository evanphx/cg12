package goc_test

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The goc suite is the tree's longest verification item, and for most of this
// project's life it was also its most serial one: 349 top-level tests, not one
// of them calling t.Parallel, on a 64-core box. Every gate was briefed to run it
// as `go test -parallel 10 ./goc/...`, which did nothing at all -- `-parallel`
// bounds tests that opt in, and none did. The suite ran at 4.0 of 64 cores for
// nineteen minutes.
//
// So every top-level test here calls t.Parallel as its first statement, except
// the ones sequential_tests.txt names and says why about. This test enforces
// both directions, because both directions are load-bearing:
//
//   - A new test added WITHOUT t.Parallel silently serializes the suite behind
//     itself. Go runs every sequential top-level test to completion before it
//     resumes any parallel one, so a sequential test does not merely run alone,
//     it runs first and everything waits.
//   - A test added to sequential_tests.txt that then GAINS a t.Parallel loses
//     the quiet process the list exists to give it -- and the placement tests
//     that need that quiet fail in a way that reads as a compiler regression.
//
// sequential_tests.txt is the single source of truth: scripts/verify.sh reads
// the same file to shard the suite, so a name added there needs no other edit.
const sequentialTestList = "sequential_tests.txt"

func TestEveryTestIsParallelOrListedAsSequential(t *testing.T) {
	t.Parallel()

	sequential := readSequentialTestList(t)
	if len(sequential) < 10 {
		t.Fatalf("%s lists %d tests; that is not the file this expects", sequentialTestList, len(sequential))
	}

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 10 {
		t.Fatalf("found %d test files; the glob is wrong", len(files))
	}

	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Recv != nil || !isTopLevelTest(function) {
				continue
			}
			name := function.Name.Name
			seen[name] = true

			parallel := callsParallelFirst(function)
			reason, listed := sequential[name]

			switch {
			case parallel && listed:
				t.Errorf("%s (%s): calls t.Parallel, but %s lists it as %s. "+
					"Remove the call, or remove the line -- a listed test that runs in "+
					"parallel loses the quiet process the list exists to give it.",
					name, filepath.Base(file), sequentialTestList, reason)
			case !parallel && !listed:
				t.Errorf("%s (%s): does not call t.Parallel as its first statement, and "+
					"%s does not list it. Add the call, or add the name to the list with "+
					"a reason tag. A sequential test runs before every parallel one, so "+
					"this is not free.",
					name, filepath.Base(file), sequentialTestList)
			}
		}
	}

	for name := range sequential {
		if !seen[name] {
			t.Errorf("%s names %s, which is not a test in this package", sequentialTestList, name)
		}
	}
}

// compilerGlobalWriters is every place in this package's tests that writes state
// the `opt` package holds for the whole process. It is frozen here because a
// write like that is not local to the test that makes it: every compile running
// anywhere in the process reads the same variable.
//
// That is not hypothetical. `TestEscapeSummaryCost` sets opt.EscapeSummaries to
// false across three whole-program compiles to price the fact table, and at
// -parallel 32 every other test compiling inside that window got the escape
// analysis with no callee summaries -- the assume-every-call-escapes arm. It
// showed up as TestAllocationCensus, TestFrameEscapeAudit and
// TestInterfaceConversionsCallTheRuntimeHelpers reporting allocations on the
// heap that a serial run keeps in a frame, and it cost a long hunt through the
// analysis, the caches and the budgets before anyone looked at the tests. See
// `analysis/escapedrift`, which reduces it to two goroutines and a handshake.
//
// So: adding a writer here is allowed, and it means the test that does it has to
// be sequential. Failing this test is the moment to decide that on purpose.
var compilerGlobalWriters = []string{
	"escapediag_test.go:TestEscapeDiagnosticDoesNotChangeTheEmittedModule",
	"escapediag_test.go:diagnoseEscapes",
	"escapesummary_test.go:TestEscapeSummaryCost",
	"escapesummary_test.go:compileCorpusForLoweringStats",
	"gcdiffreason_test.go:addGocReports",
}

func TestOnlyKnownTestsWriteProcessGlobalCompilerState(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}

	var found []string
	fset := token.NewFileSet()
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			if writesOptPackageState(function.Body) {
				found = append(found, filepath.Base(file)+":"+function.Name.Name)
			}
		}
	}
	sort.Strings(found)

	want := append([]string(nil), compilerGlobalWriters...)
	sort.Strings(want)
	if strings.Join(found, "\n") == strings.Join(want, "\n") {
		return
	}
	t.Errorf("the set of tests writing process-global opt state changed.\n"+
		"found:\n  %s\nexpected:\n  %s\n"+
		"A write to an opt package variable is seen by every compile in the process, "+
		"not only by the test that makes it. If this is a new one, the test making it "+
		"belongs in %s, and the name belongs in compilerGlobalWriters.",
		strings.Join(found, "\n  "), strings.Join(want, "\n  "), sequentialTestList)
}

// writesOptPackageState reports whether the body assigns to an `opt.X` or calls
// an `opt.SetX`. Both forms reach the same process-global variables; the setters
// only wrap theirs in an atomic.
func writesOptPackageState(body *ast.BlockStmt) bool {
	writes := false
	ast.Inspect(body, func(node ast.Node) bool {
		switch statement := node.(type) {
		case *ast.AssignStmt:
			for _, target := range statement.Lhs {
				if isPackageSelector(target, "opt") {
					writes = true
				}
			}
		case *ast.CallExpr:
			selector, ok := statement.Fun.(*ast.SelectorExpr)
			if ok && isPackageSelector(selector, "opt") && strings.HasPrefix(selector.Sel.Name, "Set") {
				writes = true
			}
		}
		return !writes
	})
	return writes
}

// isPackageSelector reports whether the expression is `pkg.Something`.
func isPackageSelector(expression ast.Expr, pkg string) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	name, ok := selector.X.(*ast.Ident)
	return ok && name.Name == pkg
}

// readSequentialTestList parses the shared list into name -> reason tag. The
// format is deliberately two tab-separated columns and nothing else, so that a
// shell script can read it with cut(1) and get the same answer this does.
func readSequentialTestList(t *testing.T) map[string]string {
	t.Helper()

	file, err := os.Open(sequentialTestList)
	if err != nil {
		t.Fatalf("open %s: %v", sequentialTestList, err)
	}
	defer file.Close()

	list := map[string]string{}
	lines := bufio.NewScanner(file)
	for number := 1; lines.Scan(); number++ {
		line := lines.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, reason, ok := strings.Cut(line, "\t")
		if !ok || name == "" || reason == "" {
			t.Fatalf("%s:%d: want '<test name>\\t<reason tag>', got %q", sequentialTestList, number, line)
		}
		switch reason {
		case "census", "placement", "setenv", "timing":
		default:
			t.Errorf("%s:%d: unknown reason tag %q for %s; the tags are documented in the file header",
				sequentialTestList, number, reason, name)
		}
		if previous, duplicate := list[name]; duplicate {
			t.Errorf("%s:%d: %s is listed twice (%s and %s)", sequentialTestList, number, name, previous, reason)
		}
		list[name] = reason
	}
	if err := lines.Err(); err != nil {
		t.Fatalf("read %s: %v", sequentialTestList, err)
	}
	return list
}

// isTopLevelTest reports whether a declaration is a `func TestX(t *testing.T)`
// that `go test` will run. Signature, not name prefix: the corpus test files are
// full of `func Test() int` inside the Go source strings they compile, and those
// are program text, not tests.
func isTopLevelTest(function *ast.FuncDecl) bool {
	if !strings.HasPrefix(function.Name.Name, "Test") || len(function.Name.Name) <= 4 {
		return false
	}
	params := function.Type.Params.List
	if len(params) != 1 {
		return false
	}
	pointer, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := pointer.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "testing" && selector.Sel.Name == "T"
}

// callsParallelFirst reports whether the function's first statement is
// t.Parallel(). First specifically: a t.Parallel after a slow setup step has
// already spent that setup serially.
func callsParallelFirst(function *ast.FuncDecl) bool {
	if function.Body == nil || len(function.Body.List) == 0 {
		return false
	}
	statement, ok := function.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := statement.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Parallel" {
		return false
	}
	receiver, ok := selector.X.(*ast.Ident)
	return ok && receiver.Name == "t"
}
