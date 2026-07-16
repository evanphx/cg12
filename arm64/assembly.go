package arm64

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/plan9asm"
)

type assemblyBundle struct {
	source     string
	references map[string]bool
	functions  []goFunctionInfo
}

func prepareAssembly(module *ir.Module) (assemblyBundle, error) {
	bundle := assemblyBundle{references: make(map[string]bool)}
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
		for _, function := range translation.Functions {
			flags := byte(goFuncFlagAsm)
			for _, flag := range function.Flags {
				if flag == "TOPFRAME" {
					flags |= goFuncFlagTopFrame
				}
			}
			bundle.functions = append(bundle.functions, goFunctionInfo{
				name:       function.Name,
				frameSize:  function.Frame,
				frameStart: function.FrameStart,
				funcID:     assemblyFunctionID(function.Name),
				funcFlag:   flags,
			})
		}
	}
	bundle.source = output.String()
	return bundle, nil
}

func assemblyFunctionID(name string) byte {
	const funcIDAsyncPreempt = 3
	if name == "runtime_asyncPreempt" {
		return funcIDAsyncPreempt
	}
	return 0
}

// TranslateAssembly parses and converts the module's Go-style Plan 9 assembly
// sources to GNU AArch64 syntax.
func TranslateAssembly(module *ir.Module) (string, error) {
	bundle, err := prepareAssembly(module)
	return bundle.source, err
}
