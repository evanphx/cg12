package amd64

import (
	"fmt"
	"sort"

	"github.com/evanphx/cg12/internal/gometa"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// finishGoModule is the Go frontend's object-module finisher, the amd64
// counterpart of arm64's. With every function's metadata facts in hand it lays
// the module data out the way the runtime describes it -- one contiguous range,
// no .bss, symbol differences resolved after layout -- records which data words
// hold pointers, and assembles pclntab and moduledata.
func finishGoModule(o *obj.Object, m *ir.Module, goFunctions []goFunctionInfo, dataDefinitions []*ir.Data, bundle assemblyBundle) error {
	moduleDataSymbol, err := goModuleDataSymbol(m, dataDefinitions)
	if err != nil {
		return err
	}
	if len(goFunctions) > 0 {
		o.Syms = append(o.Syms, obj.Sym{
			Name:    sanitize(".goc.runtime.text"),
			Section: obj.SecText,
			Value:   0,
		})
	}
	textEndSymbol := goTextEndSymbol(m)
	if len(goFunctions) > 0 && len(bundle.functions) == 0 {
		// The module's text ends where its last text is emitted. That is normally
		// the translated Plan 9 sidecar, which defines the symbol itself; a module
		// with no assembly functions has no sidecar text, so the object is the end
		// of the module and defines the bound here.
		o.Syms = append(o.Syms, obj.Sym{
			Name: textEndSymbol, Section: obj.SecText, Value: uint64(len(o.Text)), Func: true,
		})
	}
	var moduledata *ir.Data
	var dataPointerOffsets []uint64
	var typeDescriptorOffsets []uint64
	var relativeDataFixups []relativeDataFixup
	for _, d := range dataDefinitions {
		if moduleDataSymbol != "" && sanitize(d.Name) == moduleDataSymbol {
			moduledata = d
			continue
		}
		if bundle.definitions[sanitize(d.Name)] {
			continue
		}
		if err := addDataWithBSSAndFixups(o, goLinkerAligned(d), false, &relativeDataFixups); err != nil {
			return fmt.Errorf("data %s: %w", d.Name, err)
		}
		symbol := o.Syms[len(o.Syms)-1]
		if symbol.Section != obj.SecData {
			continue
		}
		offsets, err := goDataPointerOffsets(d, symbol.Value, symbol.Size)
		if err != nil {
			return fmt.Errorf("data %s: %w", d.Name, err)
		}
		dataPointerOffsets = append(dataPointerOffsets, offsets...)
		if d.GoTypeLink {
			typeDescriptorOffsets = append(typeDescriptorOffsets, symbol.Value)
		}
	}
	if err := resolveRelativeDataFixups(o, relativeDataFixups); err != nil {
		return fmt.Errorf("relative data reference: %w", err)
	}
	if moduledata == nil {
		return nil
	}
	module := gometa.Module{
		DataSymbol:      moduleDataSymbol,
		DataExport:      moduledata.Linkage.Export,
		DataStartSymbol: sanitize(".goc.runtime.datastart"),
		DataEndSymbol:   sanitize(".goc.runtime.dataend"),
		TextEndSymbol:   textEndSymbol,
		HasMain:         m.GoHasMain,
		TypeDescriptors: typeDescriptorOffsets,
		InitTaskCount:   gometa.ModuleInitTaskCount(m),
		ItabLinkCount:   gometa.ModuleItabLinkCount(m),
	}
	if len(goFunctions) == 0 && len(bundle.functions) == 0 {
		return addData(o, moduledata)
	}
	return gometa.AddObjectMetadata(goMetadataArch, gometa.Options{}, o, goFunctions, bundle.functions, module, dataPointerOffsets)
}

// goDataPointerOffsets returns the data-section offsets of the pointer words in
// one Go datum: the words the frontend declared, and every 8-byte symbol
// reference.
func goDataPointerOffsets(data *ir.Data, base, size uint64) ([]uint64, error) {
	pointers := make(map[uint64]bool)
	for _, word := range data.PointerWords {
		if word < 0 || uint64(word)*8+8 > size {
			return nil, fmt.Errorf("pointer word %d lies outside %d-byte definition", word, size)
		}
		pointers[base+uint64(word)*8] = true
	}

	cursor := uint64(0)
	for _, item := range data.Items {
		switch {
		case item.Zero > 0:
			cursor += uint64(item.Zero)
		case item.Str != "":
			cursor += uint64(len(item.Str))
		case item.Sym != "" && item.RelativeTo != "":
			cursor += uint64(item.Sub.Size())
		case item.Sym != "":
			itemSize := uint64(item.Sub.Size())
			if itemSize != 8 {
				cursor += itemSize
				continue
			}
			if cursor%8 != 0 {
				return nil, fmt.Errorf("pointer relocation at unaligned byte offset %d", cursor)
			}
			pointers[base+cursor] = true
			cursor += itemSize
		case len(item.Flts) > 0:
			cursor += uint64(len(item.Flts) * item.Sub.Size())
		default:
			cursor += uint64(len(item.Ints) * item.Sub.Size())
		}
	}

	offsets := make([]uint64, 0, len(pointers))
	for offset := range pointers {
		offsets = append(offsets, offset)
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	return offsets, nil
}

// goTextEndSymbol is the symbol that bounds this module's text.
func goTextEndSymbol(m *ir.Module) string {
	if m.GoModuleData == "" {
		return gometa.DefaultTextEndSymbol
	}
	return gometa.TextEndSymbol(sanitize(m.GoModuleData))
}

// goModuleDataSymbol is the linker name of the module's runtime.moduledata
// record, as the frontend declared it. A module that defines the runtime's own
// firstmoduledata without naming it is refused rather than left without
// metadata, which would link and then fail to start.
func goModuleDataSymbol(m *ir.Module, dataDefinitions []*ir.Data) (string, error) {
	if m.GoModuleData != "" {
		return sanitize(m.GoModuleData), nil
	}
	for _, d := range dataDefinitions {
		if sanitize(d.Name) == gometa.DefaultModuleDataSymbol {
			return "", fmt.Errorf("module defines %s but does not name it in ir.Module.GoModuleData", d.Name)
		}
	}
	return "", nil
}

// goMaxDataAlign is Go's linker's Maxalign for amd64.
const goMaxDataAlign = 32

// goLinkerAligned returns d with the alignment Go's linker would give it. A
// symbol without a stronger requirement of its own is aligned to the largest
// power of two not exceeding its size, up to 32 bytes (cmd/link's symalign).
// The runtime's amd64 assembly relies on it: aeshashbody XORs
// runtime.aeskeysched straight from memory with a legacy-SSE PXOR, which
// faults unless the operand is 16-byte aligned, and Go declares that array as
// a plain [128]byte.
func goLinkerAligned(d *ir.Data) *ir.Data {
	size := dataSize(d)
	align := goMaxDataAlign
	for align > 1 && align > size {
		align >>= 1
	}
	if align <= d.Align {
		return d
	}
	aligned := *d
	aligned.Align = align
	return &aligned
}

// dataSize is the byte size a data definition occupies.
func dataSize(d *ir.Data) int {
	size := 0
	for _, item := range d.Items {
		switch {
		case item.Zero > 0:
			size += item.Zero
		case item.Str != "":
			size += len(item.Str)
		case item.Sym != "":
			size += item.Sub.Size()
		case len(item.Flts) > 0:
			size += len(item.Flts) * item.Sub.Size()
		default:
			size += len(item.Ints) * item.Sub.Size()
		}
	}
	return size
}
