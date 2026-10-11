package plan9asm

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// This file holds the instruction-level differential check of the amd64
// translator against the Go toolchain itself: the same Plan 9 source is
// assembled once by `go tool asm` (cmd/asm + cmd/internal/obj/x86, the
// reference) and once by translating it and running the system assembler,
// and the two instruction streams are decoded by the same disassembler
// (llvm-objdump, AT&T syntax) and compared function by function.
//
// Normalization, all of it deliberate:
//   - Go inserts a stack-split check into every function that is not NOSPLIT
//     (and is not a small leaf); the translator emits none, by design (see
//     translateText). The check's leading instructions and its trailing
//     CALL runtime.morestack/JMP-to-start block are stripped from the Go side.
//   - NOPs are dropped on both sides. Both assemblers pad PCALIGN with NOPs,
//     but they pick different multi-byte NOP forms, and the padding length
//     follows from earlier encoding choices.
//   - Branch targets are compared as instruction indices within the function,
//     because encoding-length choices move absolute addresses.
//   - Relocated fields are zero in both objects, so symbol references compare
//     equal by construction; the symbol names themselves are checked by the
//     translator's unit tests instead.
//
// Anything else that differs is reported. amd64EquivalentEncodings lists the
// differences that are accepted, each with the reason.

type amd64Instruction struct {
	address uint64
	text    string
	// relocated marks an instruction with a relocation against one of its
	// fields (a symbol reference whose displacement the linker fills in).
	relocated bool
	// code is the instruction's encoding in hex.
	code string
}

func requireTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is unavailable", name)
	}
	return path
}

// requireMatchingGo returns the go command when its version is the one the
// vendored standard library (../stdlib/VERSION) comes from, so that its
// assembler is the reference for exactly these sources.
func requireMatchingGo(t *testing.T) string {
	t.Helper()
	goTool := requireTool(t, "go")
	version, err := os.ReadFile(filepath.Join("..", "stdlib", "VERSION"))
	require.NoError(t, err)
	want := strings.TrimSpace(strings.SplitN(string(version), "\n", 2)[0])
	output, err := exec.Command(goTool, "env", "GOVERSION").Output()
	if err != nil {
		t.Skipf("go env GOVERSION: %v", err)
	}
	if got := strings.TrimSpace(string(output)); got != want {
		t.Skipf("the reference assembler is %s; the vendored standard library is %s", got, want)
	}
	return goTool
}

// goAssembleAMD64 assembles one stdlib file with the Go toolchain's own
// assembler, supplying the same go_asm.h constants the translator gets.
func goAssembleAMD64(t *testing.T, goTool string, file amd64SourceFile) string {
	t.Helper()
	directory := t.TempDir()
	var header strings.Builder
	defines := amd64AssemblyDefines(t, file.packagePath)
	for _, name := range sortedKeys(defines) {
		fmt.Fprintf(&header, "#define %s %d\n", name, defines[name])
	}
	require.NoError(t, os.WriteFile(filepath.Join(directory, "go_asm.h"), []byte(header.String()), 0o644))
	source := filepath.Join("..", "stdlib", "src", filepath.FromSlash(file.packagePath), file.name)
	absoluteSource, err := filepath.Abs(source)
	require.NoError(t, err)
	runtimeDirectory, err := filepath.Abs(filepath.Join("..", "stdlib", "src", "runtime"))
	require.NoError(t, err)
	object := filepath.Join(directory, "go.o")
	arguments := []string{"tool", "asm", "-p", file.packagePath}
	arguments = append(arguments,
		"-I", directory,
		"-I", runtimeDirectory,
		"-D", "GOOS_linux", "-D", "GOARCH_amd64", "-D", "GOAMD64_v1",
		"-o", object, absoluteSource)
	command := exec.Command(goTool, arguments...)
	command.Env = append(os.Environ(), "GOARCH=amd64", "GOOS=linux", "GOAMD64=v1")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	return object
}

var goObjdumpText = regexp.MustCompile(`^TEXT (\S+)\(SB\)`)
var goObjdumpRelocation = regexp.MustCompile(`\[(\d+):\d+\]R_`)
var goObjdumpInstruction = regexp.MustCompile(`^\s+\S+:\d+\s+0x[0-9a-f]+\s+([0-9a-f]+)\s`)

// goFunctionBytes returns the machine code of every function in a Go object,
// keyed by the sanitized name the translator gives the function's body, and
// the function-relative offsets of the instructions that carry relocations.
func goFunctionBytes(t *testing.T, goTool, object, fileTag string) (map[string][]byte, map[string]map[uint64]bool, []string) {
	t.Helper()
	output, err := exec.Command(goTool, "tool", "objdump", object).CombinedOutput()
	require.NoError(t, err, "%s", output)
	functions := make(map[string][]byte)
	relocations := make(map[string]map[uint64]bool)
	var order []string
	current := ""
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if match := goObjdumpText.FindStringSubmatch(line); match != nil {
			name := match[1]
			if strings.HasSuffix(name, "<1>") {
				current = ".L" + fileTag + "_" + sanitizeSymbol(strings.TrimSuffix(name, "<1>"))
			} else {
				current = sanitizeSymbol(name)
			}
			order = append(order, current)
			functions[current] = nil
			relocations[current] = make(map[uint64]bool)
			continue
		}
		if match := goObjdumpInstruction.FindStringSubmatch(line); match != nil && current != "" {
			code, err := hexBytes(match[1])
			require.NoError(t, err, line)
			// [a:b]R_TYPE:target marks bytes a..b of this instruction as
			// relocated. Record the byte, not the instruction: Go's decoder can
			// lose instruction alignment on encodings it does not know, while
			// the bytes stay right.
			for _, relocation := range goObjdumpRelocation.FindAllStringSubmatch(line, -1) {
				start, _ := strconv.Atoi(relocation[1])
				relocations[current][uint64(len(functions[current])+start)] = true
			}
			functions[current] = append(functions[current], code...)
		}
	}
	require.NoError(t, scanner.Err())
	return functions, relocations, order
}

func hexBytes(source string) ([]byte, error) {
	if len(source)%2 != 0 {
		return nil, fmt.Errorf("odd hex length in %q", source)
	}
	code := make([]byte, len(source)/2)
	for index := range code {
		value, err := strconv.ParseUint(source[2*index:2*index+2], 16, 8)
		if err != nil {
			return nil, err
		}
		code[index] = byte(value)
	}
	return code, nil
}

var llvmInstruction = regexp.MustCompile(`^\s*([0-9a-f]+):\s+((?:[0-9a-f]{2} )+)\s*(.*)$`)
var llvmSymbol = regexp.MustCompile(`^([0-9a-f]{16})\s.*\sF\s+\.text\s+[0-9a-f]+\s+(?:\.hidden\s+)?(\S+)$`)

// disassembleAMD64 decodes an ELF object's .text with llvm-objdump in AT&T
// syntax and splits it into functions at the function symbols.
func disassembleAMD64(t *testing.T, objdump, object string) map[string][]amd64Instruction {
	t.Helper()
	symbolsOutput, err := exec.Command(objdump, "-t", object).CombinedOutput()
	require.NoError(t, err, "%s", symbolsOutput)
	type symbol struct {
		address uint64
		name    string
	}
	var symbols []symbol
	for _, line := range strings.Split(string(symbolsOutput), "\n") {
		match := llvmSymbol.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		address, err := strconv.ParseUint(match[1], 16, 64)
		require.NoError(t, err)
		symbols = append(symbols, symbol{address: address, name: match[2]})
	}
	sort.SliceStable(symbols, func(left, right int) bool {
		return symbols[left].address < symbols[right].address
	})

	output, err := exec.Command(objdump, "-d", "-r", "--x86-asm-syntax=att", object).CombinedOutput()
	require.NoError(t, err, "%s", output)
	var instructions []amd64Instruction
	for _, line := range strings.Split(string(output), "\n") {
		if llvmRelocation.MatchString(line) && len(instructions) > 0 {
			// The relocation belongs to the instruction just printed.
			instructions[len(instructions)-1].relocated = true
			continue
		}
		match := llvmInstruction.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		address, err := strconv.ParseUint(match[1], 16, 64)
		require.NoError(t, err)
		code := strings.ReplaceAll(match[2], " ", "")
		instructions = append(instructions, amd64Instruction{address: address, text: strings.TrimSpace(match[3]), code: code})
	}

	functions := make(map[string][]amd64Instruction)
	for index, symbol := range symbols {
		end := ^uint64(0)
		for _, next := range symbols[index+1:] {
			if next.address > symbol.address {
				end = next.address
				break
			}
		}
		var body []amd64Instruction
		for _, instruction := range instructions {
			if instruction.address >= symbol.address && instruction.address < end {
				body = append(body, instruction)
			}
		}
		functions[symbol.name] = body
	}
	return functions
}

var llvmRelocation = regexp.MustCompile(`^\s+[0-9a-f]+:\s+R_X86_64_`)

var llvmBranchTarget = regexp.MustCompile(`^(j[a-z]+|call[a-z]*|loop[a-z]*)\s+0x([0-9a-f]+)`)

// normalizeAMD64 drops NOPs, strips trailing comments and symbolic
// annotations, and rewrites direct branch targets as instruction indices.
func normalizeAMD64(instructions []amd64Instruction) []string {
	var kept []amd64Instruction
	indexAt := make(map[uint64]int)
	for _, instruction := range instructions {
		text := instruction.text
		if comment := strings.Index(text, "#"); comment >= 0 {
			text = strings.TrimSpace(text[:comment])
		}
		if annotation := strings.Index(text, " <"); annotation >= 0 {
			text = strings.TrimSpace(text[:annotation])
		}
		text = strings.Join(strings.Fields(text), " ")
		indexAt[instruction.address] = len(kept)
		if text == "nop" || strings.HasPrefix(text, "nop") || strings.HasPrefix(text, "data16") || strings.HasPrefix(text, "cs nop") {
			continue
		}
		kept = append(kept, amd64Instruction{address: instruction.address, text: text, relocated: instruction.relocated})
	}
	end := uint64(0)
	if len(instructions) > 0 {
		last := instructions[len(instructions)-1]
		end = last.address + 1
	}
	normalized := make([]string, len(kept))
	for index, instruction := range kept {
		text := instruction.text
		if match := llvmBranchTarget.FindStringSubmatch(text); match != nil {
			target, _ := strconv.ParseUint(match[2], 16, 64)
			// A target outside the function is a symbol reference: zero
			// displacement plus a relocation in the Go object, and either that
			// or an assemble-time resolution (for hidden symbols defined in the
			// same object) in the translation.
			label := "@ext"
			if position, ok := indexAt[target]; ok && target < end && !instruction.relocated {
				label = "@" + strconv.Itoa(position)
			}
			text = match[1] + " " + label
		}
		normalized[index] = text
	}
	return normalized
}

// stripGoStackSplit removes the stack-growth check obj6 adds to a function
// that is not NOSPLIT: the leading guard (TLS load or R14, compare, branch)
// and the trailing CALL runtime.morestack_noctxt; JMP 0 block.
func stripGoStackSplit(instructions []string) []string {
	if len(instructions) < 5 {
		return instructions
	}
	last := len(instructions) - 1
	if instructions[last] != "jmp @0" || !strings.HasPrefix(instructions[last-1], "call") {
		return instructions
	}
	block := "@" + strconv.Itoa(last-1)
	guardEnd := -1
	for index := 0; index < min(6, last-1); index++ {
		if strings.HasSuffix(instructions[index], " "+block) {
			guardEnd = index
		}
	}
	if guardEnd < 0 {
		return instructions
	}
	shift := guardEnd + 1
	var stripped []string
	for _, text := range instructions[shift : last-1] {
		if match := regexp.MustCompile(`@(\d+)$`).FindStringSubmatch(text); match != nil {
			target, _ := strconv.Atoi(match[1])
			text = strings.TrimSuffix(text, match[0]) + "@" + strconv.Itoa(target-shift)
		}
		stripped = append(stripped, text)
	}
	return stripped
}

// amd64EquivalentEncodings are instruction pairs (Go, translation) that
// decode differently but mean the same thing.
var amd64EquivalentEncodings = []struct {
	goSide      *regexp.Regexp
	translation *regexp.Regexp
	reason      string
}{}

var amd64Immediate = regexp.MustCompile(`\$(-?\d+)`)

// canonicalImmediates32 rewrites the immediates of a 32-bit operation modulo
// 2^32: Go encodes CMPL $0xffffffff, r with a 32-bit immediate where GNU as
// uses the sign-extended 8-bit -1. The operation is identical.
func canonicalImmediates32(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasSuffix(fields[0], "l") {
		return text
	}
	return amd64Immediate.ReplaceAllStringFunc(text, func(match string) string {
		value, err := strconv.ParseInt(match[1:], 10, 64)
		if err != nil {
			return match
		}
		return "$" + strconv.FormatUint(uint64(uint32(value)), 10)
	})
}

func equivalentAMD64(goText, translatedText string) bool {
	if goText == translatedText {
		return true
	}
	if canonicalImmediates32(goText) == canonicalImmediates32(translatedText) {
		return true
	}
	for _, equivalence := range amd64EquivalentEncodings {
		if equivalence.goSide.MatchString(goText) && equivalence.translation.MatchString(translatedText) {
			return true
		}
	}
	return false
}

// decodeAMD64Bytes disassembles raw machine code by assembling it as .byte
// data and running llvm-objdump over the result.
func decodeAMD64Bytes(t *testing.T, cc, objdump string, functions map[string][]byte, relocations map[string]map[uint64]bool, order []string) map[string][]amd64Instruction {
	t.Helper()
	var source strings.Builder
	labels := make(map[string]string)
	source.WriteString(".text\n")
	for index, name := range order {
		label := fmt.Sprintf("gofunction%d", index)
		labels[label] = name
		fmt.Fprintf(&source, ".global %s\n.type %s, @function\n%s:\n", label, label, label)
		code := functions[name]
		for start := 0; start < len(code); start += 16 {
			end := min(start+16, len(code))
			parts := make([]string, 0, end-start)
			for _, value := range code[start:end] {
				parts = append(parts, fmt.Sprintf("0x%02x", value))
			}
			fmt.Fprintf(&source, "\t.byte %s\n", strings.Join(parts, ", "))
		}
	}
	object := assembleAMD64(t, cc, "goreference", source.String())
	decoded := disassembleAMD64(t, objdump, object)
	byName := make(map[string][]amd64Instruction)
	for label, name := range labels {
		instructions := decoded[label]
		if len(instructions) > 0 {
			start := instructions[0].address
			for index := range instructions {
				first := instructions[index].address - start
				last := first + uint64(len(instructions[index].code)/2)
				for offset := range relocations[name] {
					if offset >= first && offset < last {
						instructions[index].relocated = true
					}
				}
			}
		}
		byName[name] = instructions
	}
	return byName
}

// amd64DifferenceReport lists the differing instructions with three lines of
// context around each.
func amd64DifferenceReport(want, got []string) string {
	length := max(len(want), len(got))
	at := func(list []string, index int) string {
		if index < len(list) {
			return list[index]
		}
		return ""
	}
	show := make([]bool, length)
	for index := 0; index < length; index++ {
		if equivalentAMD64(at(want, index), at(got, index)) {
			continue
		}
		for context := max(0, index-3); context < min(length, index+4); context++ {
			show[context] = true
		}
	}
	var report strings.Builder
	fmt.Fprintf(&report, "   (%d vs %d instructions)\n", len(want), len(got))
	shown := 0
	for index := 0; index < length; index++ {
		if !show[index] {
			continue
		}
		shown++
		if shown > 60 {
			report.WriteString("   ...\n")
			break
		}
		marker := "  "
		if !equivalentAMD64(at(want, index), at(got, index)) {
			marker = "!!"
		}
		fmt.Fprintf(&report, "%s %4d  %-48s %s\n", marker, index, at(want, index), at(got, index))
	}
	return report.String()
}

// TestAMD64TranslationMatchesGoAssembler compares every function of every
// allowlisted file against the Go assembler's encoding of the same source.
func TestAMD64TranslationMatchesGoAssembler(t *testing.T) {
	cc := requireAMD64Assembler(t)
	goTool := requireMatchingGo(t)
	objdump := requireTool(t, "llvm-objdump")
	totalFunctions := 0
	totalInstructions := 0
	accepted := make(map[string]int)
	for _, file := range amd64VerifiedFiles() {
		t.Run(file.packagePath+"/"+file.name, func(t *testing.T) {
			translation := translateAMD64SourceFile(t, file)
			object := assembleAMD64(t, cc, "translated", translation.Assembly)
			translated := disassembleAMD64(t, objdump, object)

			fileTag := sanitizeSymbol(file.packagePath + "_" + strings.TrimSuffix(file.name, ".s"))
			goObject := goAssembleAMD64(t, goTool, file)
			goCode, goRelocations, order := goFunctionBytes(t, goTool, goObject, fileTag)
			reference := decodeAMD64Bytes(t, cc, objdump, goCode, goRelocations, order)

			fileInstructions := 0
			for _, name := range order {
				body := name
				if _, ok := translated[abi0Symbol(name)]; ok {
					body = abi0Symbol(name)
				}
				if _, ok := translated[abiInternalBodySymbol(name)]; ok {
					body = abiInternalBodySymbol(name)
				}
				mine, ok := translated[body]
				require.True(t, ok, "translation has no body for %s", name)
				want := stripGoStackSplit(normalizeAMD64(reference[name]))
				got := normalizeAMD64(mine)
				totalFunctions++
				totalInstructions += len(want)
				fileInstructions += len(want)
				require.NotEmpty(t, want, "%s: Go reference decoded to nothing", name)
				mismatch := len(want) != len(got)
				for index := 0; !mismatch && index < len(want); index++ {
					if want[index] == got[index] {
						continue
					}
					if !equivalentAMD64(want[index], got[index]) {
						mismatch = true
						break
					}
					accepted[want[index]+" | "+got[index]]++
				}
				if mismatch {
					t.Errorf("%s differs from the Go assembler (go | translated):\n%s", name, amd64DifferenceReport(want, got))
				}
			}
			t.Logf("%d functions, %d instructions", len(order), fileInstructions)
		})
	}
	t.Logf("compared %d functions, %d instructions", totalFunctions, totalInstructions)
	for _, pair := range sortedKeys(accepted) {
		t.Logf("accepted equivalent encoding (%d): %s", accepted[pair], pair)
	}
}
