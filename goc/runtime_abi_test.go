package goc

import (
	"go/token"
	"go/types"
	"testing"
)

func TestPrintRuntimeABI(t *testing.T) {
	loader := newSourceLoader(token.NewFileSet())
	pkg, err := loader.Import("runtime")
	if err != nil {
		t.Fatal(err)
	}
	for _, typeName := range []string{"g", "m", "gobuf", "stack", "moduledata", "mheap", "pageAlloc", "gcControllerState"} {
		object := pkg.Scope().Lookup(typeName).(*types.TypeName)
		structure := object.Type().Underlying().(*types.Struct)
		offsets := types.SizesFor("gc", "arm64").Offsetsof(structFields(structure))
		for index := 0; index < structure.NumFields(); index++ {
			t.Logf("%s.%s=%d", typeName, structure.Field(index).Name(), offsets[index])
		}
	}
}
