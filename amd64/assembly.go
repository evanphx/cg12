package amd64

import (
	"fmt"
	"runtime"
	"sort"
	"strings"

	"github.com/evanphx/cg12/internal/gometa"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/plan9asm"
)

// assemblyBundle is everything the object driver needs to know about a
// module's Plan 9 assembly: the translated GNU source linked alongside the
// object, and the symbol facts that join the two.
type assemblyBundle struct {
	source          string
	references      map[string]bool
	abi0References  map[string]bool
	definitions     map[string]bool
	functions       []goFunctionInfo
	callConventions map[string]ir.CallConvention
}

// prepareAssembly translates the module's Plan 9 assembly to GNU syntax and
// gathers what the object driver needs from it: the symbols the assembly
// defines (and how to call them), the symbols it references, the Go metadata
// of every translated function, and the ABI0 shims for the Go functions the
// assembly calls.
func prepareAssembly(module *ir.Module) (assemblyBundle, error) {
	bundle := assemblyBundle{
		references:      make(map[string]bool),
		abi0References:  make(map[string]bool),
		definitions:     make(map[string]bool),
		callConventions: make(map[string]ir.CallConvention),
	}
	if len(module.Assembly) == 0 {
		return bundle, nil
	}

	files := make([]*plan9asm.File, len(module.Assembly))
	abiInternal := make(map[string]bool)
	for index, source := range module.Assembly {
		file, err := plan9asm.ParseWithOptions(strings.NewReader(source.Source), plan9asm.ParseOptions{
			Defines: map[string]string{
				"GOARCH_amd64":         "1",
				"GOOS_" + runtime.GOOS: "1",
			},
			Includes: source.Includes,
		})
		if err != nil {
			return assemblyBundle{}, fmt.Errorf("parse assembly %s: %w", source.Path, err)
		}
		files[index] = file
		for _, symbol := range plan9asm.AMD64ABIInternalSymbols(file, source.PackagePath) {
			abiInternal[symbol] = true
		}
	}

	var output strings.Builder
	assemblyFunctions := make(map[string]bool)
	for index, source := range module.Assembly {
		translation, err := plan9asm.CompileAMD64(files[index], plan9asm.AMD64Options{
			PackagePath:        source.PackagePath,
			Filename:           source.Path,
			Defines:            source.Defines,
			FloatInputs:        source.FloatInputs,
			FloatOutputs:       source.FloatOutputs,
			Signatures:         translatorSignatures(source.Signatures),
			ABIInternalSymbols: abiInternal,
		})
		if err != nil {
			return assemblyBundle{}, fmt.Errorf("translate assembly %s: %w", source.Path, err)
		}
		output.WriteString(translation.Assembly)
		output.WriteByte('\n')
		for _, symbol := range translation.ExternalReferences {
			bundle.references[symbol] = true
		}
		for _, symbol := range translation.ABI0References {
			bundle.abi0References[symbol] = true
		}
		for _, symbol := range translation.GlobalDefinitions {
			bundle.definitions[symbol] = true
		}
		for _, function := range translation.Functions {
			plain := strings.TrimSuffix(function.Name, "_abi0")
			assemblyFunctions[plain] = true
			bundle.callConventions[plain] = ir.CallConvGoInternal
			flags := byte(goFuncFlagAsm)
			for _, flag := range function.Flags {
				if flag == "TOPFRAME" {
					flags |= goFuncFlagTopFrame
				}
			}
			flags |= assemblyFunctionFlags(function.Name)
			bundle.functions = append(bundle.functions, goFunctionInfo{
				Name:            function.Name,
				FrameSize:       function.Frame,
				FrameStart:      function.FrameStart,
				ArgumentSize:    function.Args,
				FuncID:          assemblyFunctionID(function.Name),
				FuncFlag:        flags,
				NoLocalPointers: function.NoLocalPointers,
			})
		}
	}
	// A Go function the assembly calls is compiled for ABIInternal, which its
	// name_abi0 shim adapts. Every function's final convention is recorded so
	// the direct calls the front end stamped can be re-stamped against it
	// (applyAssemblyCallConventions).
	for _, function := range module.Funcs {
		name := sanitize(function.Name)
		if bundle.abi0References[name] {
			function.CallConv = ir.CallConvGoInternal
		}
		bundle.callConventions[name] = function.CallConv
	}
	wrappers, wrapperFunctions, err := emitGoABI0AssemblyWrappers(module, bundle.abi0References, assemblyFunctions)
	if err != nil {
		return assemblyBundle{}, err
	}
	output.WriteString(wrappers)
	bundle.functions = append(bundle.functions, wrapperFunctions...)

	if gometa.ModuleUsesGoRuntime(module) {
		// g is reached through one thread-local word, defined once per image.
		if module.GoHasMain {
			output.WriteString(plan9asm.AMD64TLSDefinition())
		}
		// The sidecar's text is the tail of the module's text, so it defines the
		// symbol that bounds it.
		if len(bundle.functions) > 0 {
			textEnd := goTextEndSymbol(module)
			fmt.Fprintf(&output, "\n\t.text\n\t.global %s\n\t.type %s, @function\n%s:\n", textEnd, textEnd, textEnd)
		}
	}
	bundle.source = output.String()
	return bundle, nil
}

// translatorSignatures converts the frontend's ABI0 slot layouts to the
// translator's.
func translatorSignatures(signatures map[string]ir.AsmSignature) map[string]plan9asm.AsmSignature {
	converted := make(map[string]plan9asm.AsmSignature, len(signatures))
	names := make([]string, 0, len(signatures))
	for name := range signatures {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		signature := signatures[name]
		result := plan9asm.AsmSignature{
			Params:  make([]plan9asm.AsmSlot, len(signature.Params)),
			Results: make([]plan9asm.AsmSlot, len(signature.Results)),
		}
		for index, slot := range signature.Params {
			result.Params[index] = plan9asm.AsmSlot{
				Name: slot.Name, Offset: slot.Offset, Width: slot.Width,
				Float: slot.Cls.IsFloat(), Group: slot.Group,
			}
		}
		for index, slot := range signature.Results {
			result.Results[index] = plan9asm.AsmSlot{
				Name: slot.Name, Offset: slot.Offset, Width: slot.Width,
				Float: slot.Cls.IsFloat(), Group: slot.Group,
			}
		}
		converted[name] = result
	}
	return converted
}

// assemblyFunctionID is the runtime's FuncID for the translated runtime
// functions the unwinder treats specially.
func assemblyFunctionID(name string) byte {
	functionIDs := map[string]byte{
		"runtime_abort":                   1,
		"runtime_abort_abi0":              1,
		"runtime_asmcgocall_abi0":         2,
		"runtime_asyncPreempt":            3,
		"runtime_asyncPreempt_abi0":       3,
		"runtime_goexit":                  8,
		"runtime_goexit_abi0":             8,
		"runtime_gogo_abi0":               9,
		"runtime_mcall":                   12,
		"runtime_mcall_abi0":              12,
		"runtime_morestack":               13,
		"runtime_morestack_abi0":          13,
		"runtime_morestack_noctxt":        13,
		"runtime_morestack_noctxt_abi0":   13,
		"runtime_mstart_abi0":             14,
		"runtime_rt0_go_abi0":             16,
		"runtime_systemstack_abi0":        21,
		"runtime_systemstack_switch_abi0": 22,
	}
	return functionIDs[name]
}

// assemblyFunctionFlags is the runtime's FuncFlag for the translated runtime
// functions that switch stacks or begin one.
func assemblyFunctionFlags(name string) byte {
	flags := map[string]byte{
		"runtime_asmcgocall_abi0":       goFuncFlagTopFrame,
		"runtime_goexit":                goFuncFlagTopFrame,
		"runtime_goexit_abi0":           goFuncFlagTopFrame,
		"runtime_gogo_abi0":             goFuncFlagSPWrite,
		"runtime_mcall":                 goFuncFlagSPWrite,
		"runtime_mcall_abi0":            goFuncFlagSPWrite,
		"runtime_morestack":             goFuncFlagSPWrite,
		"runtime_morestack_abi0":        goFuncFlagSPWrite,
		"runtime_morestack_noctxt":      goFuncFlagSPWrite,
		"runtime_morestack_noctxt_abi0": goFuncFlagSPWrite,
		"runtime_systemstack_abi0":      goFuncFlagSPWrite,
	}
	return flags[name]
}

// applyAssemblyCallConventions re-stamps every direct call to a function whose
// convention the assembly bundle settled. goc stamps each direct call with the
// platform convention before the backend runs; a call into translated assembly,
// or to a Go function assembly also calls, has to use ABIInternal instead.
func applyAssemblyCallConventions(function *ir.Func, conventions map[string]ir.CallConvention) {
	if len(conventions) == 0 {
		return
	}
	for _, block := range function.Blocks {
		for index := range block.Instrs {
			instruction := &block.Instrs[index]
			if instruction.Op != ir.OCall || len(instruction.Args) == 0 || instruction.Args[0].Kind != ir.RefConst {
				continue
			}
			callee := function.Consts[instruction.Args[0].ID]
			if callee.Kind != ir.ConstSym {
				continue
			}
			convention, ok := conventions[sanitize(callee.Sym)]
			if !ok {
				continue
			}
			instruction.CallConv = convention
			instruction.CallConvSet = true
		}
	}
}
