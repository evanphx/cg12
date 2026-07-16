package arm64

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/plan9asm"
)

type assemblyBundle struct {
	source         string
	references     map[string]bool
	abi0References map[string]bool
	functions      []goFunctionInfo
}

func prepareAssembly(module *ir.Module) (assemblyBundle, error) {
	bundle := assemblyBundle{
		references:     make(map[string]bool),
		abi0References: make(map[string]bool),
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
		translation, err := plan9asm.CompileARM64(file, plan9asm.ARM64Options{
			PackagePath:      source.PackagePath,
			Filename:         source.Path,
			Defines:          source.Defines,
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
		for _, function := range translation.Functions {
			assemblyFunctions[function.Name] = true
			flags := byte(goFuncFlagAsm)
			for _, flag := range function.Flags {
				if flag == "TOPFRAME" {
					flags |= goFuncFlagTopFrame
				}
			}
			flags |= assemblyFunctionFlags(function.Name)
			bundle.functions = append(bundle.functions, goFunctionInfo{
				name:         function.Name,
				frameSize:    function.Frame,
				frameStart:   function.FrameStart,
				argumentSize: function.Args,
				funcID:       assemblyFunctionID(function.Name),
				funcFlag:     flags,
			})
		}
	}
	wrappers, wrapperFunctions, err := emitGoABI0AssemblyWrappers(module, bundle.abi0References, assemblyFunctions)
	if err != nil {
		return assemblyBundle{}, err
	}
	output.WriteString(wrappers)
	bundle.functions = append(bundle.functions, wrapperFunctions...)
	bundle.source = output.String()
	return bundle, nil
}

func assemblyFunctionID(name string) byte {
	functionIDs := map[string]byte{
		"runtime_asmcgocall_abi0":         2,
		"runtime_asyncPreempt":            3,
		"runtime_goexit_abi0":             8,
		"runtime_gogo_abi0":               9,
		"runtime_mcall":                   12,
		"runtime_mstart_abi0":             14,
		"runtime_systemstack_abi0":        21,
		"runtime_systemstack_switch_abi0": 22,
	}
	return functionIDs[name]
}

func assemblyFunctionFlags(name string) byte {
	flags := map[string]byte{
		"runtime_asmcgocall_abi0":  goFuncFlagTopFrame,
		"runtime_gogo_abi0":        goFuncFlagSPWrite,
		"runtime_mcall":            goFuncFlagSPWrite,
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
