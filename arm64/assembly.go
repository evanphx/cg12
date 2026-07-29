package arm64

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/plan9asm"
	plan9sem "github.com/evanphx/cg12/plan9asm/sem"
)

type assemblyBundle struct {
	source          string
	references      map[string]bool
	abi0References  map[string]bool
	definitions     map[string]bool
	functions       []goFunctionInfo
	semantic        []*plan9sem.Unit
	lowered         []*ir.Func
	loweredData     []*ir.Data
	callConventions map[string]ir.CallConvention
}

func prepareAssembly(module *ir.Module) (assemblyBundle, error) {
	bundle := assemblyBundle{
		references:      make(map[string]bool),
		abi0References:  make(map[string]bool),
		definitions:     make(map[string]bool),
		callConventions: make(map[string]ir.CallConvention),
	}
	assemblyFunctions := make(map[string]bool)
	var output strings.Builder
	for _, source := range module.Assembly {
		file, err := plan9asm.ParseWithOptions(strings.NewReader(source.Source), plan9asm.ParseOptions{
			Defines: map[string]string{
				"GOARCH_arm64":         "1",
				"GOOS_" + runtime.GOOS: "1",
			},
			Includes: source.Includes,
		})
		if err != nil {
			return assemblyBundle{}, fmt.Errorf("parse assembly %s: %w", source.Path, err)
		}
		semanticUnit, err := plan9sem.Build(file, plan9sem.Options{
			PackagePath:  source.PackagePath,
			Filename:     source.Path,
			Defines:      source.Defines,
			FloatInputs:  source.FloatInputs,
			FloatOutputs: source.FloatOutputs,
			Signatures:   semanticAssemblySignatures(source.Signatures),
		})
		if err != nil {
			return assemblyBundle{}, fmt.Errorf("analyze assembly %s: %w", source.Path, err)
		}
		bundle.semantic = append(bundle.semantic, semanticUnit)
		if semanticallyLowerAssembly(source) {
			functions, err := lowerSemanticAssembly(semanticUnit, semanticAssemblyPolicyFor(module))
			if err != nil {
				return assemblyBundle{}, fmt.Errorf("lower assembly %s: %w", source.Path, err)
			}
			bundle.lowered = append(bundle.lowered, functions...)
			for _, sourceData := range semanticUnit.Data {
				data, err := lowerSemanticData(sourceData)
				if err != nil {
					return assemblyBundle{}, fmt.Errorf("lower assembly data %s: %w", source.Path, err)
				}
				bundle.loweredData = append(bundle.loweredData, data)
				bundle.definitions[sanitize(data.Name)] = true
			}
			for _, reference := range semanticUnit.References {
				bundle.references[reference.Name] = true
			}
			for _, function := range functions {
				assemblyFunctions[function.Name] = true
				bundle.callConventions[sanitize(function.Name)] = function.CallConv
			}
			continue
		}
		translation, err := plan9asm.CompileARM64(file, plan9asm.ARM64Options{
			PackagePath:      source.PackagePath,
			Filename:         source.Path,
			Defines:          source.Defines,
			FloatInputs:      source.FloatInputs,
			FloatOutputs:     source.FloatOutputs,
			PreferDirectABI0: true,
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
			assemblyFunctions[function.Name] = true
			bundle.callConventions[strings.TrimSuffix(function.Name, "_abi0")] = ir.CallConvGoInternal
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
	// The sidecar's text is the tail of the module's text, so the sidecar defines
	// the symbol that bounds it. The name is per module: a second module sharing
	// this one would take this module's text end as its own maxpc and
	// runtime.findmoduledatap would never select it. A module whose sidecar
	// carries no functions has no tail here, and the object defines its own bound
	// instead (see arm64.finishGoModule).
	if moduleUsesGoRuntime(module) && len(bundle.functions) > 0 {
		textEnd := goTextEndSymbol(module)
		fmt.Fprintf(&output, "\n\t.text\n\t.global %s\n\t.type %s, %%function\n%s:\n", textEnd, textEnd, textEnd)
	}
	bundle.source = output.String()
	return bundle, nil
}

func semanticAssemblyPolicyFor(module *ir.Module) semanticAssemblyPolicy {
	policy := semanticAssemblyPolicy{CallConv: ir.CallConvPlatform}
	allGoInternal := len(module.Funcs) != 0
	for _, function := range module.Funcs {
		if !function.UsesGoInternalCallConvention() {
			allGoInternal = false
			break
		}
	}
	if allGoInternal {
		policy.CallConv = ir.CallConvGoInternal
	}
	policy.ManagedFrame = moduleUsesGoRuntime(module)
	return policy
}

func semanticallyLowerAssembly(source ir.AssemblyFile) bool {
	if source.PackagePath == "internal/runtime/atomic" && strings.HasSuffix(source.Path, "atomic_arm64.s") {
		return true
	}
	if source.PackagePath == "internal/cpu" && (strings.HasSuffix(source.Path, "cpu_arm64.s") || strings.HasSuffix(source.Path, "cpu.s")) {
		return true
	}
	if source.PackagePath == "internal/runtime/sys" && (strings.HasSuffix(source.Path, "dit_arm64.s") || strings.HasSuffix(source.Path, "empty.s")) {
		return true
	}
	if source.PackagePath == "runtime" && strings.HasSuffix(source.Path, "atomic_arm64.s") {
		return true
	}
	if source.PackagePath == "internal/runtime/syscall/linux" && strings.HasSuffix(source.Path, "asm_linux_arm64.s") {
		return true
	}
	return source.PackagePath == "sync/atomic" && strings.HasSuffix(source.Path, "asm.s")
}

func semanticAssemblySignatures(signatures map[string]ir.AsmSignature) map[string]plan9sem.Signature {
	semantic := make(map[string]plan9sem.Signature, len(signatures))
	for name, signature := range signatures {
		converted := plan9sem.Signature{
			Params:  make([]plan9sem.Slot, len(signature.Params)),
			Results: make([]plan9sem.Slot, len(signature.Results)),
		}
		for index, slot := range signature.Params {
			converted.Params[index] = plan9sem.Slot{
				Name: slot.Name, Offset: slot.Offset, Width: slot.Width,
				Float: slot.Cls.IsFloat(), GCRef: slot.GCRef, Group: slot.Group,
			}
		}
		for index, slot := range signature.Results {
			converted.Results[index] = plan9sem.Slot{
				Name: slot.Name, Offset: slot.Offset, Width: slot.Width,
				Float: slot.Cls.IsFloat(), GCRef: slot.GCRef, Group: slot.Group,
			}
		}
		semantic[name] = converted
	}
	return semantic
}

func assemblyFunctionID(name string) byte {
	functionIDs := map[string]byte{
		"runtime_asmcgocall_abi0":         2,
		"runtime_asyncPreempt":            3,
		"runtime_goexit":                  8,
		"runtime_goexit_abi0":             8,
		"runtime_gogo_abi0":               9,
		"runtime_mcall":                   12,
		"runtime_morestack":               13,
		"runtime_morestack_noctxt":        13,
		"runtime_mstart_abi0":             14,
		"runtime_systemstack_abi0":        21,
		"runtime_systemstack_switch_abi0": 22,
	}
	return functionIDs[name]
}

func assemblyFunctionFlags(name string) byte {
	flags := map[string]byte{
		"runtime_asmcgocall_abi0":  goFuncFlagTopFrame,
		"runtime_goexit":           goFuncFlagTopFrame,
		"runtime_goexit_abi0":      goFuncFlagTopFrame,
		"runtime_gogo_abi0":        goFuncFlagSPWrite,
		"runtime_mcall":            goFuncFlagSPWrite,
		"runtime_morestack_noctxt": goFuncFlagSPWrite,
		"runtime_systemstack_abi0": goFuncFlagSPWrite,
	}
	return flags[name]
}

// TranslateAssembly parses and converts the module's Go-style Plan 9 assembly
// sources to GNU AArch64 syntax.
func TranslateAssembly(module *ir.Module) (string, error) {
	bundle, err := prepareAssembly(module)
	return bundle.source, err
}
