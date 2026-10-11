package plan9asm

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// AMD64Options supplies the package and file identity needed to resolve Go's
// package-local and file-local assembly symbols, the go_asm.h constants the
// source refers to, and the Go signatures of the functions it defines.
type AMD64Options struct {
	PackagePath string
	Filename    string
	Defines     map[string]int64

	// FloatInputs and FloatOutputs name, per function (keyed like
	// ARM64Options: the TEXT name without its package qualifier), the ABI0
	// argument and result offsets that hold float32/float64 values. They are
	// consulted only for functions that have no entry in Signatures.
	FloatInputs  map[string][]int
	FloatOutputs map[string][]int

	// Signatures holds the ABI0 slot layout of each Go-declared assembly
	// function, keyed by its sanitized symbol (package path and name joined by
	// an underscore, e.g. "runtime_memmove"), as goc's
	// assemblyPackageSignatures produces it. A function with a signature gets
	// an ABIInternal entry built from it; one without falls back to the
	// layout inferred from its name+off(FP) accesses, like the ARM64 path.
	Signatures map[string]AsmSignature

	// ABIInternalSymbols lists sanitized symbols that some other translated
	// assembly file defines with an <ABIInternal> selector. A call from this
	// file to one of them targets the plain symbol instead of the "_abi0"
	// entry. Symbols defined that way in this file are found automatically;
	// AMD64ABIInternalSymbols collects them from a parsed file.
	ABIInternalSymbols map[string]bool
}

// AsmSignature is the ABI0 slot layout of one assembly function's Go
// declaration, as goc's assemblyPackageSignatures computes it
// (ir.AsmSignature). Offsets are ABI0 argument-area offsets (name+off(FP)).
// Consecutive slots with the same nonzero Group belong to one Go value (a
// string, slice, interface, struct, or array) and are register-assigned
// together, as Go's ABIInternal assigns an aggregate as a unit.
type AsmSignature struct {
	Params  []AsmSlot
	Results []AsmSlot
}

// AsmSlot is one scalar of a function's parameters or results.
type AsmSlot struct {
	Name   string
	Offset int
	Width  int
	Float  bool
	Group  int
}

// AMD64Function describes one translated TEXT declaration. It is the same
// type as ARM64Function so the backend can consume both translations alike.
type AMD64Function = ARM64Function

// AMD64Translation is the complete result of translating one amd64 source
// file. It has the same shape as ARM64Translation.
type AMD64Translation = ARM64Translation

// AMD64TLSSymbol is the single 8-byte thread-local variable through which the
// translated amd64 assembly reaches g. Go's linker synthesizes runtime.tlsg;
// here the backend emits AMD64TLSDefinition once per executable.
const AMD64TLSSymbol = "runtime_tlsg"

// AMD64TLSDefinition returns GNU assembly that defines AMD64TLSSymbol. Every
// TLS access in translated code is %fs:runtime_tlsg@tpoff(+off), the
// local-exec form Go's linker produces for runtime.tlsg. The runtime's settls
// points FS at &m.tls+8 and expects g at -8(FS), so this must be the only TLS
// variable in the executable's TLS block (which makes its @tpoff -8); rt0_go
// verifies that at startup and aborts otherwise.
func AMD64TLSDefinition() string {
	var output strings.Builder
	output.WriteString("\n\t.section .tbss,\"awT\",@nobits\n")
	output.WriteString("\t.balign 8\n")
	fmt.Fprintf(&output, "\t.global %s\n", AMD64TLSSymbol)
	fmt.Fprintf(&output, "\t.type %s, @object\n", AMD64TLSSymbol)
	fmt.Fprintf(&output, "\t.size %s, 8\n", AMD64TLSSymbol)
	fmt.Fprintf(&output, "%s:\n", AMD64TLSSymbol)
	output.WriteString("\t.zero 8\n")
	output.WriteString("\t.text\n")
	return output.String()
}

// supportedAMD64Files lists, per package, the assembly files go/build selects
// for GOOS=linux GOARCH=amd64 whose every instruction the translator accepts
// and whose output the system assembler accepts. A package appears only when
// all of its selected assembly is listed, because SupportsAMD64Package turns
// off the purego fallback for the whole package.
var supportedAMD64Files = map[string]map[string]bool{
	"crypto/internal/boring/sig": {
		"sig_amd64.s": true,
	},
	"crypto/internal/fips140/sha256": {
		"sha256block_amd64.s": true,
	},
	"crypto/internal/fips140/sha512": {
		"sha512block_amd64.s": true,
	},
	"crypto/md5": {
		"md5block_amd64.s": true,
	},
	"crypto/sha1": {
		"sha1block_amd64.s": true,
	},
	"hash/crc32": {
		"crc32_amd64.s": true,
	},
	"internal/abi": {
		"abi_test.s": true,
		"stub.s":     true,
	},
	"internal/bytealg": {
		"compare_amd64.s":   true,
		"count_amd64.s":     true,
		"equal_amd64.s":     true,
		"index_amd64.s":     true,
		"indexbyte_amd64.s": true,
	},
	"internal/chacha8rand": {
		"chacha8_amd64.s": true,
	},
	"internal/cpu": {
		"cpu.s":     true,
		"cpu_x86.s": true,
	},
	"internal/reflectlite": {
		"asm.s": true,
	},
	"internal/runtime/atomic": {
		"atomic_amd64.s": true,
	},
	"internal/runtime/gc/scan": {
		"expand_amd64.s": true,
		"filter_amd64.s": true,
		"scan_amd64.s":   true,
	},
	"internal/runtime/sys": {
		"empty.s": true,
	},
	"internal/runtime/syscall/linux": {
		"asm_linux_amd64.s": true,
	},
	"math": {
		"dim_amd64.s":   true,
		"exp_amd64.s":   true,
		"floor_amd64.s": true,
		"hypot_amd64.s": true,
		"log_amd64.s":   true,
	},
	"math/big": {
		"arith_amd64.s": true,
	},
	"reflect": {
		"asm_amd64.s": true,
	},
	"runtime": {
		"asm.s":              true,
		"asm_amd64.s":        true,
		"ints.s":             true,
		"memclr_amd64.s":     true,
		"memmove_amd64.s":    true,
		"preempt_amd64.s":    true,
		"rt0_linux_amd64.s":  true,
		"secret_amd64.s":     true,
		"sys_linux_amd64.s":  true,
		"test_amd64.s":       true,
		"time_linux_amd64.s": true,
	},
	"sync/atomic": {
		"asm.s": true,
	},
	"syscall": {
		"asm_linux_amd64.s": true,
	},
}

// SupportsAMD64File reports whether the translator accepts a build-selected
// package assembly file in full.
func SupportsAMD64File(packagePath, filename string) bool {
	return supportedAMD64Files[packagePath][filepath.Base(filename)]
}

// SupportsAMD64Package reports whether all assembly selected for a source
// package is expected to flow through the amd64 translator. Source loading can
// then select the package's real non-purego implementation.
func SupportsAMD64Package(packagePath string) bool {
	return len(supportedAMD64Files[packagePath]) != 0
}

// AMD64ABIInternalSymbols returns the sanitized names of the non-static
// functions a parsed file defines with an <ABIInternal> selector. A backend
// that translates several files collects these from all of them and passes
// the union as AMD64Options.ABIInternalSymbols, so that a call from one file
// to an ABIInternal function in another reaches the function directly.
func AMD64ABIInternalSymbols(file *File, packagePath string) []string {
	var names []string
	for _, statement := range file.Statements {
		text, ok := statement.(*Text)
		if !ok || text.Symbol.Static || text.Symbol.ABI != "ABIInternal" {
			continue
		}
		names = append(names, qualifiedSymbol(packagePath, text.Symbol))
	}
	sort.Strings(names)
	return names
}

func qualifiedSymbol(packagePath string, symbol Symbol) string {
	name := symbol.Name
	if strings.HasPrefix(name, "·") {
		name = packagePath + name
	}
	return sanitizeSymbol(name)
}

// CompileAMD64 converts a parsed Plan 9 amd64 source file to GNU AT&T syntax
// for the system assembler and returns the symbol metadata the backend needs
// when the generated code is assembled into a separate object.
//
// The file must have been parsed with the amd64 operand grammar
// (ParseOptions.Arch "amd64", or a GOARCH_amd64 define).
//
// Symbol contract:
//   - A function declared <ABIInternal> is emitted under its plain sanitized
//     name (runtime·memmove -> runtime_memmove) and nothing else; its body may
//     assume R14 = g and X15 = 0 on entry. The exception is one with more
//     than one result (by its signature, or else its FP accesses): its body
//     is name_abiinternal and the plain name is an adapter
//     (emitABIInternalAdapter) that returns the extra results through
//     pointers, cg12's contract. Assembly in the same file calls the body.
//   - Every other non-static function's body is emitted under name_abi0. When
//     the function has no arguments and no results, the plain name is a second
//     label on the same body. Otherwise the plain name is an ABIInternal entry
//     (emitABI0Wrapper documents its calling contract, including how results
//     after the first are returned through caller-supplied pointers).
//     syscall.rawVforkSyscall's entry is hand-built instead, because nothing
//     may live on the stack across its vfork.
//   - A file-local function or datum foo<> is .L<fileTag>_foo, where fileTag is
//     the sanitized "package_file" name; static data is local to the object.
//   - A call or tail jump from assembly targets name_abi0 unless the callee is
//     file-local or known to be <ABIInternal>; such names are reported in
//     ABI0References so the backend can provide name_abi0 shims for the
//     functions it compiles itself. A materialized address ($sym(SB), LEAQ
//     sym(SB), DATA $sym(SB)) names an ABI0 function translated from this file
//     by its _abi0 body, and anything else by its plain name.
//   - Thread-local g is reached through AMD64TLSSymbol, which
//     AMD64TLSDefinition defines.
//
// Frames follow cmd/internal/obj/x86 (obj6.go preprocess): a function that
// has a frame or makes a call, and is not NOFRAME, saves BP (PUSHQ BP; MOVQ
// SP, BP) before subtracting its declared frame; every RET restores both;
// FP and named-SP operands are resolved against the running SP adjustment,
// which PUSH, POP, and ADJSP change. No stack-split check is emitted. WRAPPER
// needs no code: obj6 in Go 1.26 no longer emits the panic-argp adjustment,
// so the flag is only reported. An instruction that writes SP adds "SPWRITE"
// to the body's Flags, as obj6 marks such functions FuncFlagSPWrite.
//
// Function metadata: each TEXT yields an entry for its body (named as above)
// whose Frame is the SP delta after the prologue (frame plus the saved BP),
// and FrameStart the byte offset where that delta is reached. A generated
// ABIInternal entry precedes it as its own entry (Calls = the body). An ABI0
// body whose plain name is an alias has no separate entry for the alias.
func CompileAMD64(file *File, options AMD64Options) (AMD64Translation, error) {
	filename := strings.TrimSuffix(filepath.Base(options.Filename), filepath.Ext(options.Filename))
	translator := &amd64Translator{
		options:        options,
		fileTag:        sanitizeSymbol(options.PackagePath + "_" + filename),
		references:     make(map[string]bool),
		abi0References: make(map[string]bool),
		abiInternal:    make(map[string]bool),
		abi0Defined:    make(map[string]bool),
		adaptedBodies:  make(map[string]string),
		data:           make(map[string][]arm64DataValue),
		floatConstants: make(map[string]string),
	}
	for name := range options.ABIInternalSymbols {
		translator.abiInternal[name] = true
	}
	if err := translator.analyze(file); err != nil {
		return AMD64Translation{}, fmt.Errorf("%s: %w", options.Filename, err)
	}
	for _, statement := range file.Statements {
		directive, ok := statement.(*Directive)
		if !ok || directive.Name != "DATA" {
			continue
		}
		if err := translator.recordData(directive); err != nil {
			return AMD64Translation{}, fmt.Errorf("%s:%d: %w", options.Filename, statement.Position().Line, err)
		}
	}
	for _, statement := range file.Statements {
		if err := translator.translate(statement); err != nil {
			return AMD64Translation{}, fmt.Errorf("%s:%d: %s: %w", options.Filename, statement.Position().Line, statementText(statement), err)
		}
	}
	if err := translator.finishFunction(); err != nil {
		return AMD64Translation{}, fmt.Errorf("%s: %w", options.Filename, err)
	}
	translator.emitFloatConstants()
	// Nothing here needs an executable stack; say so, or the linker assumes
	// the object does.
	translator.output.WriteString("\n\t.section .note.GNU-stack,\"\",@progbits\n")

	return AMD64Translation{
		Assembly:           translator.output.String(),
		ExternalReferences: sortedSet(translator.references),
		ABI0References:     sortedSet(translator.abi0References),
		GlobalDefinitions:  sortedSet(translator.globalDefinitions),
		Functions:          translator.functions,
	}, nil
}

// TranslateAMD64 converts a parsed source file to GNU assembler syntax.
func TranslateAMD64(file *File, options AMD64Options) (string, error) {
	translation, err := CompileAMD64(file, options)
	return translation.Assembly, err
}

// statementText renders a statement for error messages. Line numbers count
// the preprocessor's injected definitions and expanded includes, so the
// statement's own text is what identifies it in the source.
func statementText(statement Statement) string {
	switch statement := statement.(type) {
	case *Instruction:
		opcode := statement.Opcode
		if statement.Suffix != "" {
			opcode += "." + statement.Suffix
		}
		return strings.TrimSpace(opcode + " " + operandTexts(statement.Operands))
	case *Directive:
		return strings.TrimSpace(statement.Name + " " + operandTexts(statement.Operands))
	case *Text:
		return "TEXT " + statement.Symbol.Raw
	case *Label:
		return statement.Name + ":"
	case *Preprocessor:
		return statement.Text
	}
	return fmt.Sprintf("%T", statement)
}

func operandTexts(operands []Operand) string {
	texts := make([]string, len(operands))
	for index, operand := range operands {
		texts[index] = operand.Text
	}
	return strings.Join(texts, ", ")
}

func sortedSet(values map[string]bool) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
