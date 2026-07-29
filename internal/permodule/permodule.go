// Package permodule builds a second Go module for an image that already has
// one, so that the multi-module mechanism can be driven end to end on real goc
// output.
//
// A goc image used to be able to hold exactly one Go module, because three
// things the metadata is built around were image-wide constants: the text-end
// symbol, the moduledata symbol name, and the assumption that one type region
// spans the whole of .data. The module built here is the counterexample. It is
// an ordinary relocatable object -- compiled by arm64.CompileToObject with no
// knowledge of where it will land -- that carries:
//
//   - Go type descriptors whose NameOff/TypeOff fields are resolved against its
//     own base, exactly as goc resolves its own (ir.DataItem.RelativeTo, baked
//     by arm64.resolveRelativeDataFixups with no relocation left behind);
//   - a moduledata of its own, under its own name, with its own type region,
//     its own pclntab and its own text bounds;
//   - typelinks, so runtime.typelinksinit can give one Go type one identity
//     across the two modules;
//   - a function that is not a leaf, so a traceback and a GC stack scan have to
//     go through a frame this module's pclntab describes.
//
// Nothing here is a workaround for a compiler gap: the module is described with
// the ordinary ir vocabulary and the backend does the rest. That is the point of
// per-module type regions -- the offsets need no adjustment, because they were
// already relative to the right thing.
package permodule

import (
	"github.com/evanphx/cg12/ir"
)

// Symbol names the module defines. The ones the link step has to reach are
// exported; the rest stay local, and link.merge namespaces those per object so
// they cannot collide with the program module's own.
const (
	// DataSymbol is this module's runtime.moduledata record. It deliberately is
	// not runtime.firstmoduledata: that symbol is global and belongs to the
	// module carrying the runtime's own state.
	DataSymbol = ".goc.probe.moduledata"

	// DataStart and DataEnd bound the module's data, which is also its type
	// region. They keep goc's spelling because arm64.finishGoModule looks them up
	// by it; they are local symbols, so both modules can use the same name.
	DataStart = ".goc.runtime.datastart"
	DataEnd   = ".goc.runtime.dataend"

	// WidgetType is a named struct type with a method, a package path and a
	// PtrToThis -- one descriptor exercising NameOff, TypeOff and TextOff at once.
	WidgetType = ".goc.probe.type.Widget"

	// IntType and PointerToIntType duplicate types the program module also
	// describes. They are what makes runtime.typelinksinit do real work: IntType's
	// PtrToThis is a TypeOff into this module, and the typemap is what turns it
	// into the program module's *int rather than this module's.
	IntType          = ".goc.probe.type.int"
	PointerToIntType = ".goc.probe.type.ptrint"

	// EntryFunc is the module's first function. It is first on purpose: the
	// function at text offset 0 used to be nameless, because its name sat at
	// offset 0 of the name table and the runtime reads that as the empty string.
	EntryFunc      = ".goc.probe.entry"
	EntryFuncValue = ".goc.probe.entry.funcvalue"

	// HoldFunc is the module's non-leaf function: it holds a managed pointer
	// across an indirect call back into the program module, so a traceback and a
	// GC stack scan both have to read this module's pcsp table and locals stack
	// map.
	HoldFunc      = ".goc.probe.hold"
	HoldFuncValue = ".goc.probe.hold.funcvalue"

	// CallbackSlot holds the address of the program-module function HoldFunc
	// calls. The call is indirect through this word because the program's own
	// functions are local symbols: the link step patches the word rather than
	// resolving a name across objects.
	CallbackSlot = ".goc.probe.callback"
)

// The Go type kinds this module uses, from internal/abi.Kind. goc writes the
// same numbers (runtimeKind in goc/compile.go).
const (
	kindInt    = 2
	kindInt64  = 6
	kindFunc   = 19
	kindPtr    = 22
	kindStruct = 25
)

// internal/abi.TFlag bits.
const (
	tflagUncommon      = 1 << 0
	tflagNamed         = 1 << 2
	tflagRegularMemory = 1 << 3
	tflagDirectIface   = 1 << 5
)

// probeTextSymbol is a text symbol the Widget method entries point at. It only
// has to be a real address in the final image: the method is read through
// reflect and never called.
const probeTextSymbol = "abort"

// Options selects which of the module's parts are built. The negatives exist so
// a test can show that each piece of the mechanism is load-bearing rather than
// decorative.
type Options struct {
	// WithoutTypeLinks leaves moduledata.typelinks empty, which is what cg12
	// emitted before this package existed. runtime.typelinksinit then has nothing
	// to canonicalise, and the two modules' descriptions of one Go type stay two
	// distinct types.
	WithoutTypeLinks bool
}

// Build returns the IR for the second module.
//
// Runtime is set so the backend takes goc's data path: it keeps all-zero data in
// .data rather than moving it to .bss (a type region has to be addressable
// bytes), and it resolves the RelativeTo items after the whole module is laid
// out, so a descriptor may reference a symbol emitted after it.
//
// GoModuleData is what makes arm64.finishGoModule hand the module to
// internal/gometa: the module gets a generated pcHeader, funcnames, pctab,
// pclntable, functab, typelinks and moduledata of its own, all bounded by its own
// symbols.
func Build(options Options) *ir.Module {
	module := ir.NewModule()
	module.Runtime = true
	module.GoModuleData = DataSymbol
	// This module does not define main. The program module does, and
	// runtime.modulesinit uses the flag to put that module at the head of
	// activeModules -- which is the order runtime.typelinksinit resolves
	// duplicate descriptors in.
	module.GoHasMain = false

	addFunctions(module)

	// The module base. Every offset below is measured from here, and it is the
	// first datum emitted, so its value within this object is zero -- exactly the
	// situation goc's own datastart is in, and exactly the situation that made the
	// numbers wrong once a second object existed.
	appendData(module, DataStart, 8, byteItem(0))

	// Eight zero bytes, used as this module's gcdata/gcbss and as the GCData of
	// every pointer-free type in it.
	appendData(module, ".goc.probe.gcprog", 8, ir.DataItem{Zero: 8})

	// The word the link step patches with the program's callback.
	appendExportedData(module, CallbackSlot, 8, ir.DataItem{Sub: ir.SubL, Ints: []int64{0}})

	appendData(module, ".goc.probe.name.Widget", 1, nameItem("Widget", true))
	appendData(module, ".goc.probe.name.pkgpath", 1, nameItem("probe", false))
	appendData(module, ".goc.probe.name.Poke", 1, nameItem("Poke", true))
	appendData(module, ".goc.probe.name.int64", 1, nameItem("int64", false))
	appendData(module, ".goc.probe.name.int", 1, nameItem("int", false))
	appendData(module, ".goc.probe.name.ptrint", 1, nameItem("*int", false))
	appendData(module, ".goc.probe.name.func", 1, nameItem("func()", false))
	appendData(module, ".goc.probe.name.ptrWidget", 1, nameItem("*probe.Widget", false))
	appendData(module, ".goc.probe.name.fieldA", 1, nameItem("A", true))

	typeLinks := !options.WithoutTypeLinks

	// type int64 -- the type of Widget's one field, reached by pointer rather than
	// by offset, so it is here to show that the pointer half keeps working.
	appendTypeData(module, ".goc.probe.type.int64", typeLinks,
		typeHeader(8, 0, tflagNamed|tflagRegularMemory, 8, kindInt64, ".goc.probe.name.int64", "")...)

	// type int and type *int. Both are types the program module also describes,
	// which is the duplicate-descriptor problem per-module regions create: two
	// descriptions of one Go type. int names *int through PtrToThis, a TypeOff,
	// and that lookup is the one runtime.resolveTypeOff answers from the typemap
	// runtime.typelinksinit built.
	appendTypeData(module, IntType, typeLinks,
		typeHeader(8, 0, tflagRegularMemory, 8, kindInt, ".goc.probe.name.int", PointerToIntType)...)
	appendTypeData(module, PointerToIntType, typeLinks,
		append(
			typeHeader(8, 8, tflagDirectIface|tflagRegularMemory, 8, kindPtr, ".goc.probe.name.ptrint", ""),
			ir.DataItem{Sub: ir.SubL, Sym: IntType},
		)...)

	// func() -- the signature type a method entry names by TypeOff.
	appendTypeData(module, ".goc.probe.type.func", typeLinks,
		append(
			typeHeader(8, 8, tflagDirectIface, 8, kindFunc, ".goc.probe.name.func", ""),
			// FuncType's tail: InCount, OutCount, then padding to 8.
			ir.DataItem{Sub: ir.SubUH, Ints: []int64{0, 0}},
			ir.DataItem{Zero: 4},
		)...)

	// Widget's one StructField: {Name *Name, Typ *Type, Offset uintptr}. All three
	// are pointers, not offsets.
	appendData(module, ".goc.probe.type.Widget.fields", 8,
		ir.DataItem{Sub: ir.SubL, Sym: ".goc.probe.name.fieldA"},
		ir.DataItem{Sub: ir.SubL, Sym: ".goc.probe.type.int64"},
		ir.DataItem{Sub: ir.SubL, Ints: []int64{0}},
	)

	widget := typeHeader(8, 0, tflagUncommon|tflagNamed|tflagRegularMemory, 8, kindStruct,
		".goc.probe.name.Widget", ".goc.probe.type.ptrWidget")
	widget = append(widget,
		// StructType's tail: PkgPath Name, Fields []StructField.
		ir.DataItem{Sub: ir.SubL, Sym: ".goc.probe.name.pkgpath"},
		ir.DataItem{Sub: ir.SubL, Sym: ".goc.probe.type.Widget.fields"},
		ir.DataItem{Sub: ir.SubL, Ints: []int64{1, 1}},
		// UncommonType: PkgPath NameOff, Mcount, Xcount, Moff, unused. Moff is
		// measured from the start of the UncommonType record, and the method array
		// follows it immediately, so it is the record's own size.
		ir.DataItem{Sub: ir.SubW, Sym: ".goc.probe.name.pkgpath", RelativeTo: DataStart},
		ir.DataItem{Sub: ir.SubUH, Ints: []int64{1, 1}},
		ir.DataItem{Sub: ir.SubW, Ints: []int64{16, 0}},
		// Method: Name NameOff, Mtyp TypeOff, Ifn TextOff, Tfn TextOff.
		ir.DataItem{Sub: ir.SubW, Sym: ".goc.probe.name.Poke", RelativeTo: DataStart},
		ir.DataItem{Sub: ir.SubW, Sym: ".goc.probe.type.func", RelativeTo: DataStart},
		ir.DataItem{Sub: ir.SubW, Sym: probeTextSymbol},
		ir.DataItem{Sub: ir.SubW, Sym: probeTextSymbol},
	)
	appendExportedTypeData(module, WidgetType, typeLinks, widget...)

	// *Widget -- named by Widget's PtrToThis, which is a TypeOff.
	appendTypeData(module, ".goc.probe.type.ptrWidget", typeLinks,
		append(
			typeHeader(8, 8, tflagDirectIface, 8, kindPtr, ".goc.probe.name.ptrWidget", ""),
			ir.DataItem{Sub: ir.SubL, Sym: WidgetType},
		)...)

	// Words holding each function's address: dereferencing one as a Go func value
	// is how the program calls into this module.
	appendExportedData(module, EntryFuncValue, 8, ir.DataItem{Sub: ir.SubL, Sym: EntryFunc})
	appendExportedData(module, HoldFuncValue, 8, ir.DataItem{Sub: ir.SubL, Sym: HoldFunc})

	// The end of this module's data. It has to be the last datum: internal/gometa
	// reads it as the module's edata, etypes and end.
	appendData(module, DataEnd, 8, byteItem(0))

	// The moduledata placeholder. internal/gometa replaces these bytes with the
	// real 592-byte record and generates the pclntab that describes this module.
	appendData(module, DataSymbol, 8, ir.DataItem{Zero: moduledataSize})
	return module
}

// moduledataSize is runtime.moduledata's size on a 64-bit target. Only the
// placeholder's length; internal/gometa writes the record.
const moduledataSize = 592

// addFunctions gives the module real code.
//
// EntryFunc is a leaf, and is emitted first so that it lands at text offset 0 --
// the slot whose name used to be unreadable. HoldFunc is not a leaf: it takes a
// managed pointer, calls back into the program module through a patched word,
// and returns the pointer. The pointer is therefore live across a call in a frame
// only this module's pclntab describes, which is what makes a traceback read this
// module's pcsp table and a GC stack scan read its locals stack map.
func addFunctions(module *ir.Module) {
	entry := module.NewFunc(EntryFunc, ir.ClsW)
	entry.Export()
	entry.CallConv = ir.CallConvGoInternal
	entry.ManagedFrame = true
	entry.Entry().Ret(entry.Word(7))

	hold := module.NewFunc(HoldFunc, ir.ClsM)
	hold.Export()
	hold.CallConv = ir.CallConvGoInternal
	hold.ManagedFrame = true
	held := hold.Param("held", ir.ClsM)
	body := hold.Entry()
	callback := body.Load(ir.ClsP, hold.Sym(CallbackSlot, 0))
	body.CallVoidWithConvention(ir.CallConvGoInternal, callback)
	body.Ret(held)
}

// typeHeader writes the 48-byte internal/abi.Type common prefix, in goc's own
// field order (goc/compile.go's ensureTypeTag).
//
// Hash is written as zero because that is what goc writes. It only buckets
// runtime.typelinksinit's candidate list; runtime.typesEqual is the actual
// equality test, so a constant hash costs a longer scan and nothing else.
func typeHeader(size, ptrBytes, tflag, align int64, kind int64, nameSymbol, ptrToThisSymbol string) []ir.DataItem {
	pointerToThis := ir.DataItem{Sub: ir.SubW, Ints: []int64{0}}
	if ptrToThisSymbol != "" {
		pointerToThis = ir.DataItem{Sub: ir.SubW, Sym: ptrToThisSymbol, RelativeTo: DataStart}
	}
	return []ir.DataItem{
		{Sub: ir.SubL, Ints: []int64{size, ptrBytes}},
		{Sub: ir.SubW, Ints: []int64{0}},
		{Sub: ir.SubUB, Ints: []int64{tflag, align, align, kind}},
		{Sub: ir.SubL, Ints: []int64{0}}, // Equal: nil, nothing here is compared
		{Sub: ir.SubL, Sym: ".goc.probe.gcprog"},
		{Sub: ir.SubW, Sym: nameSymbol, RelativeTo: DataStart},
		pointerToThis,
	}
}

// nameItem encodes an internal/abi.Name: a flag byte, a varint length, and the
// bytes. It is goc's runtimeNameBytes for the tagless case.
func nameItem(name string, exported bool) ir.DataItem {
	var flags byte
	if exported {
		flags |= 1 << 0
	}
	encoded := []byte{flags}
	for length := len(name); ; {
		value := byte(length & 0x7f)
		length >>= 7
		if length == 0 {
			encoded = append(encoded, value)
			break
		}
		encoded = append(encoded, value|0x80)
	}
	encoded = append(encoded, name...)
	return ir.DataItem{Sub: ir.SubUB, Str: string(encoded)}
}

func byteItem(value int64) ir.DataItem {
	return ir.DataItem{Sub: ir.SubUB, Ints: []int64{value}}
}

func appendData(module *ir.Module, name string, align int, items ...ir.DataItem) {
	module.Data = append(module.Data, &ir.Data{Name: name, Align: align, Items: items})
}

func appendExportedData(module *ir.Module, name string, align int, items ...ir.DataItem) {
	data := &ir.Data{Name: name, Align: align, Items: items}
	data.Linkage.Export = true
	module.Data = append(module.Data, data)
}

func appendTypeData(module *ir.Module, name string, typeLink bool, items ...ir.DataItem) {
	module.Data = append(module.Data, &ir.Data{Name: name, Align: 8, Items: items, GoTypeLink: typeLink})
}

func appendExportedTypeData(module *ir.Module, name string, typeLink bool, items ...ir.DataItem) {
	data := &ir.Data{Name: name, Align: 8, Items: items, GoTypeLink: typeLink}
	data.Linkage.Export = true
	module.Data = append(module.Data, data)
}
