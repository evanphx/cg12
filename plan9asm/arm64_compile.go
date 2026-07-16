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
	PackagePath      string
	Filename         string
	Defines          map[string]int64
	PreferDirectABI0 bool
}

// ARM64Function describes one translated TEXT declaration in source order.
type ARM64Function struct {
	Name       string
	Frame      int
	FrameStart int
	Flags      []string
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
	"internal/cpu": {
		"cpu_arm64.s": true,
	},
	"internal/chacha8rand": {
		"chacha8_arm64.s": true,
	},
	"internal/runtime/sys": {
		"dit_arm64.s": true,
	},
	"internal/runtime/syscall/linux": {
		"asm_linux_arm64.s": true,
	},
	"internal/runtime/atomic": {
		"atomic_arm64.s": true,
	},
	"runtime": {
		"atomic_arm64.s":    true,
		"memclr_arm64.s":    true,
		"memmove_arm64.s":   true,
		"preempt_arm64.s":   true,
		"secret_arm64.s":    true,
		"sys_linux_arm64.s": true,
		"tls_arm64.s":       true,
	},
	"syscall": {
		"asm_linux_arm64.s": true,
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
	filename := strings.TrimSuffix(filepath.Base(options.Filename), filepath.Ext(options.Filename))
	translator := arm64Translator{
		options:     options,
		fileTag:     sanitizeSymbol(options.PackagePath + "_" + filename),
		labels:      make(map[int]map[string]string),
		references:  make(map[string]bool),
		abi0Layouts: collectABI0Layouts(file),
		data:        make(map[string][]arm64DataValue),
	}
	translator.directABI0 = collectDirectABI0(file, translator.abi0Layouts)
	for _, statement := range file.Statements {
		directive, ok := statement.(*Directive)
		if !ok || directive.Name != "DATA" {
			continue
		}
		if err := translator.recordData(directive); err != nil {
			return ARM64Translation{}, fmt.Errorf("%s:%d: %w", options.Filename, statement.Position().Line, err)
		}
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
