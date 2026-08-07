package main

import (
	"fmt"
	"reflect"
	"unsafe"

	"github.com/evanphx/cg12/ir"
)

func dump(name string, v any) {
	t := reflect.TypeOf(v)
	fmt.Printf("=== %s: %d bytes ===\n", name, t.Size())
	prev := uintptr(0)
	prevName := ""
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if gap := f.Offset - prev; gap > 0 && prevName != "" {
			fmt.Printf("      ---- %d bytes padding after %s\n", gap, prevName)
		}
		fmt.Printf("  %4d %3d  %s\n", f.Offset, f.Type.Size(), f.Name)
		prev = f.Offset + f.Type.Size()
		prevName = f.Name
	}
	if gap := t.Size() - prev; gap > 0 {
		fmt.Printf("      ---- %d bytes tail padding\n", gap)
	}
}

func main() {
	dump("Instr", ir.Instr{})
	fmt.Printf("\nBlock %d  Func %d  Jmp %d\n", unsafe.Sizeof(ir.Block{}), unsafe.Sizeof(ir.Func{}), unsafe.Sizeof(ir.Jmp{}))
}
