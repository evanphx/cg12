package goc

import (
	"testing"
)

func TestDiscoverPackageTests(t *testing.T) {
	tests, err := discoverPackageTests("container/list")
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
