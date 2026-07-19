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
	FloatInputs      map[string][]int
	FloatOutputs     map[string][]int
	PreferDirectABI0 bool
}

// ARM64Function describes one translated TEXT declaration in source order.
type ARM64Function struct {
	Name            string
	Frame           int
	FrameStart      int
	Args            int
	Flags           []string
	NoLocalPointers bool
}

// ARM64Translation is the complete result of parsing one source file for the
// GNU assembler and the cg12 object emitter.
type ARM64Translation struct {
	Assembly           string
	ExternalReferences []string
	ABI0References     []string
	GlobalDefinitions  []string
	Functions          []ARM64Function
}

var supportedARM64Files = map[string]map[string]bool{
	"crypto/md5": {
		"md5block_arm64.s": true,
	},
	"crypto/sha1": {
		"sha1block_arm64.s": true,
	},
	"hash/crc32": {
		"crc32_arm64.s": true,
	},
	"crypto/internal/fips140/sha256": {
		"sha256block_arm64.s": true,
	},
	"crypto/internal/fips140/sha512": {
		"sha512block_arm64.s": true,
	},
	"crypto/internal/boring/sig": {
		"sig_other.s": true,
	},
	"internal/bytealg": {
		"compare_arm64.s":   true,
		"count_arm64.s":     true,
		"equal_arm64.s":     true,
		"index_arm64.s":     true,
		"indexbyte_arm64.s": true,
	},
	"internal/abi": {
		"abi_test.s": true,
		"stub.s":     true,
	},
	"internal/cpu": {
		"cpu.s":       true,
		"cpu_arm64.s": true,
	},
	"internal/chacha8rand": {
		"chacha8_arm64.s": true,
	},
	"internal/runtime/sys": {
		"dit_arm64.s": true,
		"empty.s":     true,
	},
	"internal/runtime/syscall/linux": {
		"asm_linux_arm64.s": true,
	},
	"internal/runtime/atomic": {
		"atomic_arm64.s": true,
	},
	"math": {
		"dim_arm64.s":   true,
		"exp_arm64.s":   true,
		"floor_arm64.s": true,
	},
	"math/big": {
		"arith_arm64.s": true,
	},
	"internal/reflectlite": {
		"asm.s": true,
	},
	"reflect": {
		"asm_arm64.s": true,
	},
	"runtime": {
		"asm.s":             true,
		"asm_arm64.s":       true,
		"atomic_arm64.s":    true,
		"ints.s":            true,
		"memclr_arm64.s":    true,
		"memmove_arm64.s":   true,
		"preempt_arm64.s":   true,
		"rt0_linux_arm64.s": true,
		"secret_arm64.s":    true,
		"sys_linux_arm64.s": true,
		"tls_arm64.s":       true,
	},
	"syscall": {
		"asm_linux_arm64.s": true,
	},
	"sync/atomic": {
		"asm.s": true,
	},
}

// SupportsARM64File reports whether the translator currently accepts a
// build-selected package assembly file in full.
func SupportsARM64File(packagePath, filename string) bool {
	return supportedARM64Files[packagePath][filepath.Base(filename)]
}

// SupportsARM64Package reports whether all assembly selected for a source
// package is expected to flow through the ARM64 translator. Source loading can
// then select the package's real non-purego implementation.
func SupportsARM64Package(packagePath string) bool {
	return len(supportedARM64Files[packagePath]) != 0
}

// CompileARM64 converts a parsed Plan 9 ARM64 source file to GNU syntax and
// returns the symbol metadata needed when the generated code is assembled into
// a separate object.
func CompileARM64(file *File, options ARM64Options) (ARM64Translation, error) {
	filename := strings.TrimSuffix(filepath.Base(options.Filename), filepath.Ext(options.Filename))
	translator := arm64Translator{
		options:           options,
		fileTag:           sanitizeSymbol(options.PackagePath + "_" + filename),
		labels:            make(map[int]map[string]string),
		pcLabels:          make(map[int]map[int]string),
		instructionIndex:  make(map[*Instruction]int),
		references:        make(map[string]bool),
		abi0References:    make(map[string]bool),
		abi0Layouts:       collectABI0Layouts(file, options),
		abi0Symbols:       make(map[string]bool),
		directABI0Symbols: make(map[string]bool),
		functionCalls:     make(map[int]bool),
		data:              make(map[string][]arm64DataValue),
	}
	translator.directABI0 = collectDirectABI0(file, translator.abi0Layouts)
	globalDefinitions := make(map[string]bool)
	functionIndex := -1
	for _, statement := range file.Statements {
		switch statement := statement.(type) {
		case *Text:
			functionIndex++
			if options.PackagePath == "runtime" && filepath.Base(options.Filename) == "preempt_arm64.s" && translator.symbol(statement.Symbol) == "runtime_asyncPreempt" {
				// asyncPreempt is entered by compiler-injected asynchronous
				// preemption calls using ABIInternal even though its assembly
				// declaration has no explicit ABI selector.
				translator.directABI0[functionIndex] = true
			}
			if !statement.Symbol.Static && statement.Symbol.ABI == "" {
				translator.abi0Symbols[translator.symbol(statement.Symbol)] = true
			}
			if translator.directABI0[functionIndex] && !statement.Symbol.Static && statement.Symbol.ABI == "" {
				translator.directABI0Symbols[translator.symbol(statement.Symbol)] = true
			}
		case *Instruction:
			if statement.Opcode == "BL" || statement.Opcode == "CALL" {
				translator.functionCalls[functionIndex] = true
			}
		case *Directive:
			if statement.Name != "GLOBL" || len(statement.Operands) == 0 {
				continue
			}
			symbol, ok := operandSymbol(statement.Operands[0])
			if ok && !symbol.Static {
				globalDefinitions[translator.symbol(symbol)] = true
			}
		}
	}
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
	if err := translator.collectPCLabels(file); err != nil {
		return ARM64Translation{}, fmt.Errorf("%s: %w", options.Filename, err)
	}

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
	abi0References := make([]string, 0, len(translator.abi0References))
	for symbol := range translator.abi0References {
		abi0References = append(abi0References, symbol)
	}
	sort.Strings(abi0References)
	definitions := make([]string, 0, len(globalDefinitions))
	for symbol := range globalDefinitions {
		definitions = append(definitions, symbol)
	}
	sort.Strings(definitions)
	return ARM64Translation{
		Assembly:           translator.output.String(),
		ExternalReferences: references,
		ABI0References:     abi0References,
		GlobalDefinitions:  definitions,
		Functions:          translator.functions,
	}, nil
}

// TranslateARM64 converts a parsed source file to GNU assembler syntax.
func TranslateARM64(file *File, options ARM64Options) (string, error) {
	translation, err := CompileARM64(file, options)
	return translation.Assembly, err
}
