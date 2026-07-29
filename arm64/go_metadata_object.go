package arm64

import (
	"fmt"

	"github.com/evanphx/cg12/internal/gometa"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// The Go object metadata is emitted by internal/gometa, which is
// architecture-neutral; this file is what remains once the neutral part is
// factored out. It holds arm64's parameter set, its one private PCDATA table,
// and the two hooks that join the neutral emitter to this backend's emit loop
// (goFunctionInfoFor) and object driver (finishGoModule).
//
// The facts structs are used pervasively across the arm64 emit loop, so they
// keep their local spellings as aliases rather than forcing every call site to
// name the shared package.
type goFunctionInfo = gometa.FunctionInfo
type goStackMapPoint = gometa.StackMapPoint

// goMetadataArch is arm64's instantiation of the shared emitter. PC deltas are
// counted in 4-byte instructions, absolute data references use the AArch64
// ABS32/ABS64 relocations, and a managed frame opens with the saved {FP, LR}
// pair, so the first local sits 16 bytes above the frame base.
var goMetadataArch = gometa.Arch{
	PCQuantum:      4,
	Reloc32:        obj.R_AARCH64_ABS32,
	Reloc64:        obj.R_AARCH64_ABS64,
	FrameBaseBytes: 16,
}

// goMetadataOptions adds arm64's private PCDATA table to the Go-defined slots.
func goMetadataOptions() gometa.Options {
	return gometa.Options{ExtraPCData: goAAPCSFrameSlotPCData}
}

const (
	goFuncFlagTopFrame = gometa.FuncFlagTopFrame
	goFuncFlagSPWrite  = gometa.FuncFlagSPWrite
	goFuncFlagAsm      = gometa.FuncFlagAsm
)

const goModuleInitTasksName = gometa.ModuleInitTasksName
const goModuleItabLinksName = gometa.ModuleItabLinksName

// goFunctionInfoFor gathers the Go runtime metadata facts for one emitted
// function -- frame geometry, defer-return offset, stack-map pointer words, and
// argument-register pointer map -- that the Go module finisher later assembles
// into pclntab and moduledata. It is the sole Go-specific step in the otherwise
// frontend-neutral per-function emit loop.
func goFunctionInfoFor(f *ir.Func, name string, mc *machineCode) (goFunctionInfo, error) {
	deferReturn, err := mc.m.deferReturnOffset()
	if err != nil {
		return goFunctionInfo{}, err
	}
	argumentFrame := goArgumentFrame{}
	if f.UsesManagedFrame() {
		argumentFrame = goArgumentFrameFor(f)
	}
	return goFunctionInfo{
		Name:                          name,
		FrameSize:                     mc.m.frame,
		FrameStart:                    mc.m.frameStart,
		ManagedFrame:                  f.UsesManagedFrame(),
		OutgoingSize:                  mc.m.outgoing,
		Size:                          uint64(len(mc.code)),
		FuncID:                        gometa.RuntimeFunctionID(name),
		DeferReturn:                   deferReturn,
		LocalPointerWords:             mc.m.goLocalPointerWords(),
		StackMapPoints:                mc.m.goStackMapPoints(),
		ArgumentSize:                  argumentFrame.size,
		ArgumentPointerWords:          argumentFrame.pointerWords,
		SafepointArgumentPointerWords: argumentFrame.incomingPointerWords,
	}, nil
}

// finishGoModule is the Go frontend's object-module finisher. With the neutral
// emit loop's per-function facts (goFunctions) in hand, it appends the runtime
// text symbol, lays the module data out with the runtime's BSS and relative-fixup
// layout while recording data pointer offsets, and assembles pclntab/moduledata.
// It is the Go-specific counterpart to addNeutralData, kept out of the neutral
// object driver.
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
		// of the module and defines the bound here. Without this an object-only Go
		// module would reference a text-end symbol nobody defines -- or, worse,
		// pick up another module's.
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
		if bundle.definitions[sanitize(d.Name)] && !semanticDataDefinition(bundle.loweredData, d) {
			continue
		}
		if err := addDataWithBSSAndFixups(o, d, false, &relativeDataFixups); err != nil {
			return fmt.Errorf("data %s: %w", d.Name, err)
		}
		symbol := o.Syms[len(o.Syms)-1]
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
	return addGoRuntimeObjectMetadata(o, goFunctions, bundle.functions, moduledata, module, dataPointerOffsets)
}

// goTextEndSymbol is the symbol that bounds this module's text. A Go module that
// declares no moduledata emits no metadata either and so nothing references the
// symbol; it keeps the default name rather than one derived from an empty one.
func goTextEndSymbol(m *ir.Module) string {
	if m.GoModuleData == "" {
		return gometa.DefaultTextEndSymbol
	}
	return gometa.TextEndSymbol(sanitize(m.GoModuleData))
}

// goModuleDataSymbol is the linker name of the module's runtime.moduledata
// record, as the frontend declared it.
//
// An image may carry more than one Go module, so the record's name is a property
// of the module rather than a constant this backend matches. A module that
// defines the runtime's own firstmoduledata without saying so is refused: the
// alternative is silently emitting no metadata at all for it, which produces an
// image that links and then cannot start.
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

func moduleUsesGoRuntime(module *ir.Module) bool {
	return gometa.ModuleUsesGoRuntime(module)
}

// addGoRuntimeObjectMetadata hands the module to the shared emitter, except for
// a module with no functions at all: it needs no pclntab, and its moduledata is
// emitted as plain data through this backend's own data path.
func addGoRuntimeObjectMetadata(object *obj.Object, functions, translatedFunctions []goFunctionInfo, moduledata *ir.Data, module gometa.Module, pointerOffsets []uint64) error {
	if len(functions) == 0 && len(translatedFunctions) == 0 {
		return addData(object, moduledata)
	}
	return gometa.AddObjectMetadata(goMetadataArch, goMetadataOptions(), object, functions, translatedFunctions, module, pointerOffsets)
}

// goAAPCSFrameSlotPCData adapts goAAPCSFramePCData to the shared emitter's
// extra-PCDATA hook, which is what places the table at PCDATA slot 5 and raises
// the _func record's npcdata to 6.
func goAAPCSFrameSlotPCData(function gometa.FunctionInfo) []byte {
	outgoingSize := -1
	if function.ManagedFrame {
		outgoingSize = function.OutgoingSize
	}
	return goAAPCSFramePCData(function.FrameStart, outgoingSize)
}

// goAAPCSFramePCData describes cg12's managed AAPCS frame record to the copied
// runtime. PCDATA slots 0-4 are defined by Go; cg12 reserves slot 5 for the
// outgoing area between the physical SP and the saved {FP, LR} pair. A value
// of -1 identifies functions that retain the standard Go assembly frame shape.
func goAAPCSFramePCData(frameStart, outgoingSize int) []byte {
	if outgoingSize < 0 {
		return goMetadataArch.PCValueTable(0, -1)
	}
	return goMetadataArch.PCValueTable(frameStart, outgoingSize)
}
