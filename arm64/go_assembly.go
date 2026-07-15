package arm64

import (
	"fmt"
	"strings"

	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/plan9asm"
)

// TranslateAssembly parses and converts the module's Go-style Plan 9 assembly
// sources to GNU AArch64 syntax.
func TranslateAssembly(module *ir.Module) (string, error) {
	var output strings.Builder
	for _, source := range module.Assembly {
		file, err := plan9asm.Parse(strings.NewReader(source.Source))
		if err != nil {
			return "", fmt.Errorf("parse assembly %s: %w", source.Path, err)
		}
		assembly, err := plan9asm.TranslateARM64(file, plan9asm.ARM64Options{
			PackagePath: source.PackagePath,
			Filename:    source.Path,
		})
		if err != nil {
			return "", fmt.Errorf("translate assembly %s: %w", source.Path, err)
		}
		output.WriteString(assembly)
		output.WriteByte('\n')
	}
	return output.String(), nil
}

func assemblyExternalReferences(module *ir.Module) (map[string]bool, error) {
	references := make(map[string]bool)
	for _, source := range module.Assembly {
		file, err := plan9asm.Parse(strings.NewReader(source.Source))
		if err != nil {
			return nil, fmt.Errorf("parse assembly %s: %w", source.Path, err)
		}
		options := plan9asm.ARM64Options{
			PackagePath: source.PackagePath,
			Filename:    source.Path,
		}
		for _, symbol := range plan9asm.ARM64ExternalReferences(file, options) {
			references[symbol] = true
		}
	}
	return references, nil
}
