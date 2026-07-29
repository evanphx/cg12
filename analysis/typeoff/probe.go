package main

import (
	"github.com/evanphx/cg12/ir"
)

// This file builds the prototype's second module: a relocatable object holding
// Go type descriptors and a `moduledata` of its own, compiled with nothing but
// the ordinary `ir` data vocabulary and the arm64 backend.
//
// The point of building it separately rather than carving it out of a goc
// object is that this is what separate compilation actually looks like. Every
// NameOff/TypeOff below is written as
// `ir.DataItem{Sub: ir.SubW, Sym: X, RelativeTo: probeDataStart}`, which is the
// same construct goc uses at its seven sites, and the backend resolves it the
// same way -- `value(X) - value(probeDataStart)`, baked into the bytes with no
// relocation left behind. The module has no idea where in the final image it
// will land, and never learns.
//
// What makes that legal is the second half of the change: the module carries
// its own `moduledata` whose `types`/`etypes` bound *its* data, and the runtime
// resolves an offset against the module that contains the referring type
// (runtime/type.go:resolveNameOff, :resolveTypeOff). So the baked numbers stay
// correct no matter how far the linker moves this module's data.

// The Go type kinds this module uses, from internal/abi.Kind. goc writes the
// same numbers (goc/compile.go:12365).
const (
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

// moduledataSize is runtime.moduledata's size on a 64-bit target, and the field
// offsets the record is filled at. They are constants for the same reason
// internal/gometa treats them as constants: the record has the same layout on
// every 64-bit architecture.
const (
	moduledataSize      = 592
	moduledataNextField = 584
	moduledataEtypes    = 304
)

// Symbol names this module defines. Only the ones the link step has to reach
// are exported; everything else stays local, which is what lets the module
// reuse goc's own `.goc.runtime.datastart` spelling in `-functions` mode --
// link.merge namespaces every local symbol by its object, so the two bases
// cannot collide.
const (
	probeWidgetType = ".goc.probe.type.Widget"
	probeEntryFunc  = ".goc.probe.entry"
	probeFillerFunc = ".goc.probe.filler"
	probeEntryValue = ".goc.probe.entry.funcvalue"
)

// probeTextSymbol is a text symbol the probe module's method entries point at.
// It only has to be a real address in the final image: the prototype reads the
// method's Tfn through reflect but never calls it.
const probeTextSymbol = "abort"

// probeLayout names the symbols a built probe module defines, since two of them
// differ between the two shapes the module can take.
type probeLayout struct {
	module     *ir.Module
	dataStart  string
	dataEnd    string
	moduleData string
	// withFunctions is set when the module carries real code and lets
	// internal/gometa build its pclntab and moduledata, rather than carrying a
	// hand-written function-less moduledata.
	withFunctions bool
}

// buildProbeModule returns the IR for the second module.
//
// Runtime is set so the backend takes goc's data path: it keeps all-zero data
// in .data rather than moving it to .bss (the type region has to be addressable
// bytes), and it resolves the RelativeTo items after the whole module is laid
// out, which is what lets a descriptor reference a symbol emitted after it.
//
// With withFunctions false the module defines no functions, and its moduledata
// is the hand-written record in probeModuleDataItems. With it true the module
// defines a function and names its moduledata `runtime.firstmoduledata`, which
// is what makes `arm64.finishGoModule` hand it to `internal/gometa` -- so the
// module gets a real, generated pclntab of its own. That is the shape a
// prebuilt-runtime split would actually produce.
func buildProbeModule(withFunctions bool) probeLayout {
	layout := probeLayout{
		module:        ir.NewModule(),
		dataStart:     ".goc.probe.datastart",
		dataEnd:       ".goc.probe.dataend",
		moduleData:    ".goc.probe.moduledata",
		withFunctions: withFunctions,
	}
	if withFunctions {
		// gometa looks these two up by their exact goc names, and emits the
		// moduledata for the definition named `runtime.firstmoduledata`. All
		// three are local symbols, so the linker namespaces them per object and
		// the goc module's own copies are untouched.
		layout.dataStart = ".goc.runtime.datastart"
		layout.dataEnd = ".goc.runtime.dataend"
		layout.moduleData = "runtime.firstmoduledata"
	}
	probeDataStart := layout.dataStart
	module := layout.module
	module.Runtime = true

	if withFunctions {
		// One real function, so internal/gometa builds this module a genuine
		// pcHeader/funcnames/pctab/pclntable/functab and a moduledata that
		// bounds this module's own text. It is exported so the link step can
		// point the program at it, and it does nothing: the point is that the
		// runtime can find it and name it through *this* module's tables.
		// Two functions, not one. `runtime.moduledata.funcName` treats a name
		// offset of 0 as the empty string, and internal/gometa lays the first
		// function's name at offset 0 of `.goc.go.funcnames` with no leading
		// sentinel byte -- so the first function of any goc module is nameless
		// to the runtime (see the report's "a defect this spike turned up").
		// The probe uses the second function so the prototype measures the
		// module split rather than that bug.
		filler := module.NewFunc(probeFillerFunc, ir.ClsW).Export()
		filler.Entry().Ret(filler.Word(0))
		function := module.NewFunc(probeEntryFunc, ir.ClsW).Export()
		entry := function.Entry()
		entry.Ret(function.Word(7))
	}

	// The module base. Every offset below is measured from here, and it is the
	// first datum emitted, so its value within this object is zero -- exactly
	// the situation goc's `.goc.runtime.datastart` is in, and exactly the
	// situation that used to make the numbers wrong once a second object
	// existed.
	appendData(module, probeDataStart, 8, byteItem(0))

	// Eight zero bytes, used as this module's gcdata/gcbss and as the GCData of
	// every pointer-free type in it.
	appendData(module, ".goc.probe.gcprog", 8, ir.DataItem{Zero: 8})

	appendData(module, ".goc.probe.name.Widget", 1, nameItem("Widget", true))
	appendData(module, ".goc.probe.name.pkgpath", 1, nameItem("probe", false))
	appendData(module, ".goc.probe.name.Poke", 1, nameItem("Poke", true))
	appendData(module, ".goc.probe.name.int64", 1, nameItem("int64", false))
	appendData(module, ".goc.probe.name.func", 1, nameItem("func()", false))
	appendData(module, ".goc.probe.name.ptrWidget", 1, nameItem("*probe.Widget", false))
	appendData(module, ".goc.probe.name.fieldA", 1, nameItem("A", true))

	// type int64 -- the type of Widget's one field, reached by pointer rather
	// than by offset, so it is here to show that the pointer half keeps working.
	appendData(module, ".goc.probe.type.int64", 8,
		typeHeader(probeDataStart, 8, 0, 0x1064, tflagNamed|tflagRegularMemory, 8, kindInt64,
			".goc.probe.name.int64", "")...)

	// func() -- the signature type a method entry names by TypeOff.
	appendData(module, ".goc.probe.type.func", 8,
		append(
			typeHeader(probeDataStart, 8, 8, 0x1019, tflagDirectIface, 8, kindFunc, ".goc.probe.name.func", ""),
			// FuncType's tail: InCount, OutCount, then padding to 8.
			ir.DataItem{Sub: ir.SubUH, Ints: []int64{0, 0}},
			ir.DataItem{Zero: 4},
		)...)

	// Widget's one StructField: {Name *Name, Typ *Type, Offset uintptr}. All
	// three are pointers, not offsets.
	appendData(module, ".goc.probe.type.Widget.fields", 8,
		ir.DataItem{Sub: ir.SubL, Sym: ".goc.probe.name.fieldA"},
		ir.DataItem{Sub: ir.SubL, Sym: ".goc.probe.type.int64"},
		ir.DataItem{Sub: ir.SubL, Ints: []int64{0}},
	)

	widget := typeHeader(probeDataStart, 8, 0, 0x1025, tflagUncommon|tflagNamed|tflagRegularMemory, 8, kindStruct,
		".goc.probe.name.Widget", ".goc.probe.type.ptrWidget")
	widget = append(widget,
		// StructType's tail: PkgPath *Name, Fields []StructField.
		ir.DataItem{Sub: ir.SubL, Sym: ".goc.probe.name.pkgpath"},
		ir.DataItem{Sub: ir.SubL, Sym: ".goc.probe.type.Widget.fields"},
		ir.DataItem{Sub: ir.SubL, Ints: []int64{1, 1}},
		// UncommonType: PkgPath NameOff, Mcount, Xcount, Moff, unused. Moff is
		// measured from the start of the UncommonType record, and the method
		// array follows it immediately, so it is the record's own size.
		ir.DataItem{Sub: ir.SubW, Sym: ".goc.probe.name.pkgpath", RelativeTo: probeDataStart},
		ir.DataItem{Sub: ir.SubUH, Ints: []int64{1, 1}},
		ir.DataItem{Sub: ir.SubW, Ints: []int64{16, 0}},
		// Method: Name NameOff, Mtyp TypeOff, Ifn TextOff, Tfn TextOff.
		ir.DataItem{Sub: ir.SubW, Sym: ".goc.probe.name.Poke", RelativeTo: probeDataStart},
		ir.DataItem{Sub: ir.SubW, Sym: ".goc.probe.type.func", RelativeTo: probeDataStart},
		ir.DataItem{Sub: ir.SubW, Sym: probeTextSymbol},
		ir.DataItem{Sub: ir.SubW, Sym: probeTextSymbol},
	)
	appendExportedData(module, probeWidgetType, 8, widget...)

	// *Widget -- named by Widget's PtrToThis, which is a TypeOff, and so is the
	// field the prototype uses to prove TypeOff resolution.
	appendData(module, ".goc.probe.type.ptrWidget", 8,
		append(
			typeHeader(probeDataStart, 8, 8, 0x1022, tflagDirectIface, 8, kindPtr, ".goc.probe.name.ptrWidget", ""),
			ir.DataItem{Sub: ir.SubL, Sym: probeWidgetType},
		)...)

	if withFunctions {
		// A word holding the probe function's address: dereferencing it as a Go
		// func value is how the program calls into the second module.
		appendExportedData(module, probeEntryValue, 8, ir.DataItem{Sub: ir.SubL, Sym: probeEntryFunc})
		// The end of this module's data. It has to be the last datum, because
		// gometa reads it as the module's edata/etypes/end.
		appendData(module, layout.dataEnd, 8, byteItem(0))
		// Named `runtime.firstmoduledata` so arm64.finishGoModule hands the
		// module to internal/gometa, which replaces these placeholder items with
		// a real 592-byte record and generates the pclntab that describes it.
		// The symbol stays local, so the linker namespaces it away from the
		// program's own.
		appendData(module, layout.moduleData, 8, ir.DataItem{Zero: moduledataSize})
		return layout
	}

	// The minimum pclntab a function-less module needs to pass
	// runtime.moduledataverify1: a well-formed pcHeader and a one-entry functab.
	// minpc == maxpc == 0, so runtime.findfunc never selects it.
	appendData(module, ".goc.probe.pcheader", 8,
		ir.DataItem{Sub: ir.SubW, Ints: []int64{0xfffffff1}},
		ir.DataItem{Sub: ir.SubUB, Ints: []int64{0, 0, 4, 8}}, // pad1, pad2, minLC, ptrSize
		ir.DataItem{Sub: ir.SubL, Ints: []int64{0, 0, 0, 0, 0, 0, 0, 0}},
	)
	appendData(module, ".goc.probe.pclntable", 8, ir.DataItem{Zero: 8})
	appendData(module, ".goc.probe.functab", 8, ir.DataItem{Zero: 8})
	appendData(module, ".goc.probe.findfunctab", 8, ir.DataItem{Zero: 20})

	appendExportedData(module, layout.moduleData, 8, probeModuleDataItems(layout.dataStart, layout.dataEnd)...)

	// The end of this module's type region. Exported so the flat-scheme control
	// can point the goc module's `etypes` at it.
	appendExportedData(module, layout.dataEnd, 8, byteItem(0))
	return layout
}

// probeModuleDataItems writes the 592-byte runtime.moduledata record.
//
// The two fields that matter to this prototype are `types` and `etypes`: they
// bound this module's own data, so runtime.resolveNameOff and
// runtime.resolveTypeOff pick this module for any offset read out of a
// descriptor in it, and add the offset to this module's base rather than the
// program's.
//
// The rest is the minimum that keeps runtime.moduledataverify1 and
// runtime.modulesinit happy for a module with no functions and no globals to
// scan: an empty [data, edata) range, and a pre-set (empty, non-zero)
// gcdatamask so modulesinit does not try to run a GC program over it.
func probeModuleDataItems(probeDataStart, probeDataEnd string) []ir.DataItem {
	zero := func(n int) ir.DataItem { return ir.DataItem{Sub: ir.SubL, Ints: make([]int64, n)} }
	pointer := func(name string) ir.DataItem { return ir.DataItem{Sub: ir.SubL, Sym: name} }
	emptySlice := func() []ir.DataItem { return []ir.DataItem{zero(3)} }

	items := []ir.DataItem{
		pointer(".goc.probe.pcheader"), // pcHeader
	}
	items = append(items, emptySlice()...) // funcnametab
	items = append(items, emptySlice()...) // cutab
	items = append(items, emptySlice()...) // filetab
	items = append(items, emptySlice()...) // pctab
	items = append(items,
		pointer(".goc.probe.pclntable"), // pclntable
		ir.DataItem{Sub: ir.SubL, Ints: []int64{8, 8}},
		pointer(".goc.probe.functab"), // ftab: one entry, so nftab is 0
		ir.DataItem{Sub: ir.SubL, Ints: []int64{1, 1}},
		pointer(".goc.probe.findfunctab"),
		zero(2),                      // minpc, maxpc
		zero(2),                      // text, etext
		pointer(probeDataStart),      // noptrdata
		pointer(probeDataStart),      // enoptrdata
		pointer(probeDataStart),      // data
		pointer(probeDataStart),      // edata: empty, so no globals to scan
		pointer(probeDataStart),      // bss
		pointer(probeDataStart),      // ebss
		pointer(probeDataStart),      // noptrbss
		pointer(probeDataStart),      // enoptrbss
		zero(2),                      // covctrs, ecovctrs
		pointer(probeDataEnd),        // end
		pointer(".goc.probe.gcprog"), // gcdata
		pointer(".goc.probe.gcprog"), // gcbss
		pointer(probeDataStart),      // types
		pointer(probeDataEnd),        // etypes
		pointer(probeDataStart),      // rodata
		zero(2),                      // gofunc, epclntab
	)
	items = append(items, emptySlice()...) // textsectmap
	items = append(items, emptySlice()...) // typelinks
	items = append(items, emptySlice()...) // itablinks
	items = append(items, emptySlice()...) // ptab
	items = append(items, zero(2))         // pluginpath string
	items = append(items, emptySlice()...) // pkghashes
	items = append(items, emptySlice()...) // inittasks
	items = append(items, zero(2))         // modulename string
	items = append(items, emptySlice()...) // modulehashes
	items = append(items,
		// hasmain, bad, then padding to the next 8-byte boundary.
		ir.DataItem{Sub: ir.SubUB, Ints: []int64{0, 0}},
		ir.DataItem{Zero: 6},
		// gcdatamask and gcbssmask: {n int32, bytedata *byte}. Setting bytedata
		// makes them differ from the zero bitvector, which is how modulesinit
		// decides a module's masks are already built.
		ir.DataItem{Sub: ir.SubW, Ints: []int64{0, 0}},
		pointer(".goc.probe.gcprog"),
		ir.DataItem{Sub: ir.SubW, Ints: []int64{0, 0}},
		pointer(".goc.probe.gcprog"),
		zero(1), // typemap
		zero(1), // next
	)
	return items
}

// typeHeader writes the 48-byte internal/abi.Type common prefix, in goc's own
// field order (goc/compile.go:5617).
func typeHeader(base string, size, ptrBytes, hash, tflag, align int64, kind int64, nameSymbol, ptrToThisSymbol string) []ir.DataItem {
	pointerToThis := ir.DataItem{Sub: ir.SubW, Ints: []int64{0}}
	if ptrToThisSymbol != "" {
		pointerToThis = ir.DataItem{Sub: ir.SubW, Sym: ptrToThisSymbol, RelativeTo: base}
	}
	return []ir.DataItem{
		{Sub: ir.SubL, Ints: []int64{size, ptrBytes}},
		{Sub: ir.SubW, Ints: []int64{hash}},
		{Sub: ir.SubUB, Ints: []int64{tflag, align, align, kind}},
		{Sub: ir.SubL, Ints: []int64{0}}, // Equal: nil, nothing here is compared
		{Sub: ir.SubL, Sym: ".goc.probe.gcprog"},
		{Sub: ir.SubW, Sym: nameSymbol, RelativeTo: base},
		pointerToThis,
	}
}

// nameItem encodes an internal/abi.Name: a flag byte, a varint length, and the
// bytes. It is goc's runtimeNameBytes (goc/compile.go:6244) for the tagless case.
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
