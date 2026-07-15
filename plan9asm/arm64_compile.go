package plan9asm

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// ARM64Options supplies the package and file identity needed to resolve Go's
// package-local and file-local assembly symbols.
type ARM64Options struct {
	PackagePath string
	Filename    string
}

// ARM64Function describes one translated TEXT declaration in source order.
type ARM64Function struct {
	Name  string
	Frame int
	Flags []string
}

// ARM64Translation is the complete result of parsing one source file for the
// GNU assembler and the cg12 object emitter.
type ARM64Translation struct {
	Assembly           string
	ExternalReferences []string
	Functions          []ARM64Function
}

var supportedARM64Files = map[string]map[string]bool{
	"internal/bytealg": {
		"compare_arm64.s":   true,
		"count_arm64.s":     true,
		"equal_arm64.s":     true,
		"index_arm64.s":     true,
		"indexbyte_arm64.s": true,
	},
	"runtime": {
		"atomic_arm64.s":  true,
		"memclr_arm64.s":  true,
		"memmove_arm64.s": true,
	},
}

// SupportsARM64File reports whether the translator currently accepts a
// build-selected package assembly file in full.
func SupportsARM64File(packagePath, filename string) bool {
	return supportedARM64Files[packagePath][filepath.Base(filename)]
}

// CompileARM64 converts a parsed Plan 9 ARM64 source file to GNU syntax and
// returns the symbol metadata needed when the generated code is assembled into
// a separate object.
func CompileARM64(file *File, options ARM64Options) (ARM64Translation, error) {
	translator := arm64Translator{
		options:    options,
		fileTag:    sanitizeSymbol(strings.TrimSuffix(filepath.Base(options.Filename), filepath.Ext(options.Filename))),
		labels:     make(map[int]map[string]string),
		references: make(map[string]bool),
	}
	translator.collectLabels(file)

	for _, statement := range file.Statements {
		if err := translator.translate(statement); err != nil {
			return ARM64Translation{}, fmt.Errorf("%s:%d: %w", options.Filename, statement.Position().Line, err)
		}
	}

	references := make([]string, 0, len(translator.references))
	for symbol := range translator.references {
		references = append(references, symbol)
	}
	sort.Strings(references)
	return ARM64Translation{
		Assembly:           translator.output.String(),
		ExternalReferences: references,
		Functions:          translator.functions,
	}, nil
}

// TranslateARM64 converts a parsed source file to GNU assembler syntax.
func TranslateARM64(file *File, options ARM64Options) (string, error) {
	translation, err := CompileARM64(file, options)
	return translation.Assembly, err
}
