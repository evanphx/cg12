// Program typeoff_probe is the user half of the per-module type-region
// prototype (analysis/typeoff). It is compiled by goc exactly as any capability
// program is; nothing about it knows that a second module exists.
//
// probeSlot is a package-level word that the prototype's link step patches with
// the address of a type descriptor that lives in a *separately compiled*
// object -- one whose data lands megabytes away from this program's, and whose
// 32-bit NameOff/TypeOff fields were resolved against its own module base, not
// this one's. Forging an interface value around that pointer and asking the
// reflect package about it drives runtime.resolveNameOff, resolveTypeOff and
// resolveTextOff, which is the mechanism under test: each picks the module that
// contains the referring type and adds the offset to *that* module's base.
//
// Every line printed below is read through one of those resolvers, so a base
// taken from the wrong module shows up as garbage or a throw, not as a silently
// equal answer.
package main

import (
	"reflect"
	"runtime"
	"unsafe"
)

// probeSlot holds the address of the foreign module's type descriptor. It is
// zero as compiled; the prototype writes a relocation over it at link time.
var probeSlot uintptr

// probeFuncSlot and probeCodeSlot are patched only when the second module was
// built with real code (`-functions`): the first with the address of a word
// holding that module's function entry, which is what a Go func value points
// at, and the second with the entry address itself. They are how the program
// reaches a PC that only the *second* module's pclntab describes.
var probeFuncSlot uintptr
var probeCodeSlot uintptr

// emptyInterface is the layout of an interface value with no methods: a type
// descriptor and a data word.
type emptyInterface struct {
	typ  unsafe.Pointer
	data unsafe.Pointer
}

var holder emptyInterface
var storage int64 = 42

func main() {
	if probeSlot == 0 {
		println("probe: slot was never patched")
		return
	}
	holder.typ = unsafe.Pointer(probeSlot)
	holder.data = unsafe.Pointer(&storage)
	foreign := reflect.TypeOf(*(*any)(unsafe.Pointer(&holder)))

	// Str: a NameOff into the foreign module.
	println("name:", foreign.Name())
	println("string:", foreign.String())
	println("kind:", int(foreign.Kind()))
	println("size:", int(foreign.Size()))

	// UncommonType.PkgPath: a second NameOff.
	println("pkgpath:", foreign.PkgPath())

	// StructField.Name and .Typ are pointers, not offsets: they must survive too.
	println("fields:", foreign.NumField())
	field := foreign.Field(0)
	println("field0:", field.Name, int(field.Type.Kind()))

	// PtrToThis: a TypeOff into the foreign module.
	pointer := reflect.PointerTo(foreign)
	println("ptr:", pointer.String())
	println("ptr-elem:", pointer.Elem().Name())

	// Method.Name is a NameOff, Method.Mtyp a TypeOff, Method.Tfn a TextOff.
	println("methods:", foreign.NumMethod())
	method := foreign.Method(0)
	println("method:", method.Name, int(method.Type.Kind()), method.Type.NumIn(), method.Type.NumOut())

	// The second module's own pclntab: a PC that only its tables describe.
	// runtime.FuncForPC walks the module list to find the owning module and then
	// reads that module's funcnametab, so a correct name here means the second
	// module's generated metadata is being used, not the program's.
	if probeCodeSlot != 0 {
		function := runtime.FuncForPC(probeCodeSlot)
		if function == nil {
			println("foreign-func: unresolved")
		} else {
			println("foreign-func:", function.Name())
		}
	}
	if probeFuncSlot != 0 {
		value := probeFuncSlot
		call := *(*func() int32)(unsafe.Pointer(&value))
		println("foreign-call:", call())
	}

	println("probe: done")
}
