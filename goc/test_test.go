package goc

import (
	"testing"

	"github.com/evanphx/cg12/ir"
)

func TestDiscoverPackageTests(t *testing.T) {
	tests, _, err := discoverPackageTests(HostTarget(), "container/list")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"TestExtending": true,
		"TestList":      true,
		"TestRemove":    true,
	}
	for _, test := range tests {
		delete(want, test.Name)
	}
	for name := range want {
		t.Errorf("container/list test %s was not discovered", name)
	}
}

func TestDiscoverExternalPackageTests(t *testing.T) {
	tests, hasExternalTests, err := discoverPackageTests(HostTarget(), "sort")
	if err != nil {
		t.Fatal(err)
	}
	if !hasExternalTests {
		t.Fatal("sort external tests were not loaded")
	}
	want := map[string]bool{
		"TestSortIntSlice": true,
		"TestSearch":       true,
		"TestFind":         true,
	}
	for _, test := range tests {
		if test.PackagePath != "sort_test" {
			continue
		}
		delete(want, test.Name)
	}
	for name := range want {
		t.Errorf("sort external test %s was not discovered", name)
	}
}

func TestCompilePackageTests(t *testing.T) {
	module, tests, err := CompileTestExecutable("container/list")
	if err != nil {
		t.Fatal(err)
	}
	if module == nil {
		t.Fatal("test module is nil")
	}
	foundMatchStringDispatch := false
	foundBoolFlagImplementation := false
	for _, function := range module.Funcs {
		switch function.Name {
		case "testing.testDeps.MatchString":
			foundMatchStringDispatch = true
		case "flag.boolValue.IsBoolFlag":
			foundBoolFlagImplementation = true
		}
	}
	if !foundMatchStringDispatch {
		t.Fatal("testing.testDeps.MatchString interface dispatch was not compiled")
	}
	if !foundBoolFlagImplementation {
		t.Fatal("flag.boolValue.IsBoolFlag interface implementation was not compiled")
	}
	foundRemove := false
	for _, test := range tests {
		if test.Name == "TestRemove" {
			foundRemove = true
			break
		}
	}
	if !foundRemove || len(tests) <= 1 {
		t.Fatalf("compiled tests = %#v; want all container/list tests", tests)
	}
}

func TestMatchingPackageTestsSelectsTopLevelRunPattern(t *testing.T) {
	tests := []PackageTest{
		{Name: "TestAlpha", PackagePath: "example"},
		{Name: "TestBeta", PackagePath: "example"},
		{Name: "TestGamma", PackagePath: "example"},
	}

	selected, err := matchingPackageTests(tests, "^(TestAlpha|TestGamma)$/subtest")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected[0].Name != "TestAlpha" || selected[1].Name != "TestGamma" {
		t.Fatalf("selected tests = %#v", selected)
	}
}

func TestMatchingPackageTestsRejectsInvalidPattern(t *testing.T) {
	_, err := matchingPackageTests(nil, "[")
	if err == nil {
		t.Fatal("invalid pattern unexpectedly succeeded")
	}
}

func TestCompileUTF8TestsKeepsFunctionSizeBounded(t *testing.T) {
	module, _, err := CompileTestExecutable("unicode/utf8")
	if err != nil {
		t.Fatal(err)
	}

	var largestName string
	largestTemporaries := 0
	largestBlocks := 0
	largestInstructions := 0
	signalHandlerAllocates := false
	stackScannerAllocates := false
	runeCountCopiesString := false
	runeCountBorrowsString := false
	for _, function := range module.Funcs {
		instructions := 0
		for _, block := range function.Blocks {
			instructions += len(block.Instrs)
			for index := range block.Instrs {
				instruction := &block.Instrs[index]
				if instruction.Op != ir.OCall {
					continue
				}
				callee := instruction.Arg(0)
				if callee.Kind != ir.RefConst {
					continue
				}
				constant := function.Consts[callee.ID]
				if constant.Kind != ir.ConstSym || constant.Sym != "runtime.newobject" {
					if function.Name == "unicode/utf8.RuneCount" {
						switch constant.Sym {
						case "runtime.slicebytetostring":
							runeCountCopiesString = true
						case "runtime.slicebytetostringtmp":
							runeCountBorrowsString = true
						}
					}
					continue
				}
				switch function.Name {
				case "runtime.sighandler":
					signalHandlerAllocates = true
				case "runtime.stackScanState.getPtr":
					stackScannerAllocates = true
				}
			}
		}
		if len(function.Temps) > largestTemporaries {
			largestName = function.Name
			largestTemporaries = len(function.Temps)
			largestBlocks = len(function.Blocks)
			largestInstructions = instructions
		}
	}

	if largestTemporaries > 100000 {
		t.Fatalf("function %s expanded to %d temporaries, %d blocks, and %d instructions", largestName, largestTemporaries, largestBlocks, largestInstructions)
	}
	if signalHandlerAllocates {
		t.Fatal("runtime.sighandler allocates on the signal stack")
	}
	if stackScannerAllocates {
		t.Fatal("runtime.stackScanState.getPtr allocates its range literal on the heap")
	}
	if runeCountCopiesString || !runeCountBorrowsString {
		t.Fatal("unicode/utf8.RuneCount does not use a non-allocating temporary string conversion")
	}
}
