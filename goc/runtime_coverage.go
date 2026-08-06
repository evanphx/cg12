package goc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/evanphx/cg12/ir"
)

const (
	runtimeCoverageVersion    = 1
	runtimeCoverageHeaderData = ".goc.runtime.coverage.header"
	runtimeCoverageFooterData = ".goc.runtime.coverage.footer"
	runtimeCoverageDump       = ".goc.runtime.coverage.dump"
	runtimeCoverageChunkSize  = 4096
)

var (
	runtimeCoverageHeader = []byte("\nCG12-RUNTIME-COVERAGE-1\n")
	runtimeCoverageFooter = []byte("\nCG12-RUNTIME-COVERAGE-END\n")
)

// RuntimeCoverage describes one instrumented executable. Only source files
// selected for the current GOOS and GOARCH are included.
type RuntimeCoverage struct {
	Version         int                       `json:"version"`
	GOOS            string                    `json:"goos"`
	GOARCH          string                    `json:"goarch"`
	RuntimeSourceID string                    `json:"runtime_source_id"`
	Scope           string                    `json:"scope"`
	AssemblyFiles   []string                  `json:"uninstrumented_assembly_files,omitempty"`
	Functions       []RuntimeCoverageFunction `json:"functions"`
	Blocks          []RuntimeCoverageBlock    `json:"blocks"`
}

// RuntimeCoverageFunction is a function body in an active runtime Go file.
// Compiled is false when the function was not reachable in this executable.
type RuntimeCoverageFunction struct {
	Name     string `json:"name"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Compiled bool   `json:"compiled"`
	Entry    int    `json:"entry"`
}

// RuntimeCoverageBlock maps a byte in the emitted bitmap to a runtime basic
// block. ID is the byte offset within the bitmap, not within the data symbol.
type RuntimeCoverageBlock struct {
	ID       int    `json:"id"`
	Function string `json:"function"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Block    int    `json:"block"`
}

// RuntimeCoverageResult is the decoded bitmap from an instrumented process.
type RuntimeCoverageResult struct {
	Hits   []bool
	Output []byte
}

// ExtractRuntimeCoverage removes and decodes the last coverage packet in
// output. Program output before and after the packet is preserved.
func ExtractRuntimeCoverage(output []byte, coverage *RuntimeCoverage) (RuntimeCoverageResult, error) {
	if coverage == nil {
		return RuntimeCoverageResult{}, fmt.Errorf("goc: nil runtime coverage metadata")
	}
	start := bytes.LastIndex(output, runtimeCoverageHeader)
	if start < 0 {
		return RuntimeCoverageResult{}, fmt.Errorf("goc: runtime coverage packet was not written")
	}
	bitmapStart := start + len(runtimeCoverageHeader)
	bitmapEnd := bitmapStart + len(coverage.Blocks)
	packetEnd := bitmapEnd + len(runtimeCoverageFooter)
	if packetEnd > len(output) || !bytes.Equal(output[bitmapEnd:packetEnd], runtimeCoverageFooter) {
		return RuntimeCoverageResult{}, fmt.Errorf("goc: incomplete runtime coverage packet")
	}

	hits := make([]bool, len(coverage.Blocks))
	for index, value := range output[bitmapStart:bitmapEnd] {
		switch value {
		case 0:
		case 1:
			hits[index] = true
		default:
			return RuntimeCoverageResult{}, fmt.Errorf("goc: invalid runtime coverage byte %d at block %d", value, index)
		}
	}

	cleanOutput := make([]byte, 0, len(output)-(packetEnd-start))
	cleanOutput = append(cleanOutput, output[:start]...)
	cleanOutput = append(cleanOutput, output[packetEnd:]...)
	return RuntimeCoverageResult{Hits: hits, Output: cleanOutput}, nil
}

func instrumentRuntimeCoverage(
	module *ir.Module,
	target Target,
	unit *sourceUnit,
	coverage *RuntimeCoverage,
	linkNames map[*types.Func]string,
	initSymbols map[*types.Func]string,
) error {
	if runtime.GOOS != "linux" || target != TargetARM64 {
		return fmt.Errorf("goc: runtime coverage currently supports linux/arm64, not %s/%s", runtime.GOOS, target)
	}
	if unit == nil {
		return fmt.Errorf("goc: runtime coverage requested without a runtime source unit")
	}

	coverage.Version = runtimeCoverageVersion
	coverage.GOOS = runtime.GOOS
	// The recorded GOARCH describes the instrumented binary, so it is the target.
	coverage.GOARCH = target.GOARCH()
	runtimeSourceID, err := activeRuntimeSourceID(unit)
	if err != nil {
		return err
	}
	coverage.RuntimeSourceID = runtimeSourceID
	coverage.Scope = "build-selected runtime Go source functions and emitted cg12 basic blocks"
	coverage.AssemblyFiles = coverage.AssemblyFiles[:0]
	for _, assembly := range unit.assembly {
		coverage.AssemblyFiles = append(coverage.AssemblyFiles, assembly.path)
	}
	sort.Strings(coverage.AssemblyFiles)
	coverage.Functions = activeRuntimeFunctions(unit, linkNames, initSymbols)
	coverage.Blocks = nil

	activeFiles := make(map[string]bool)
	for _, sourceFile := range unit.files {
		position := unitPosition(unit, sourceFile.Pos())
		activeFiles[canonicalCoveragePath(position.Filename)] = true
	}

	functionIndex := make(map[string]int)
	for index, function := range coverage.Functions {
		functionIndex[function.Name] = index
	}

	for _, function := range module.Funcs {
		if function.Name == runtimeCoverageDump {
			continue
		}
		position, ok := runtimeFunctionPosition(module, function, activeFiles)
		if !ok {
			continue
		}

		metadataIndex, found := functionIndex[function.Name]
		if found {
			coverage.Functions[metadataIndex].Compiled = true
		}
		entryID := -1
		for blockIndex, block := range function.Blocks {
			blockPosition := runtimeBlockPosition(module, block, position, activeFiles)
			if len(block.Instrs) == 0 {
				continue
			}
			id := len(coverage.Blocks)
			if block == function.Entry() {
				entryID = id
			}
			coverage.Blocks = append(coverage.Blocks, RuntimeCoverageBlock{
				ID:       id,
				Function: function.Name,
				File:     runtimeCoverageFile(blockPosition.Filename),
				Line:     blockPosition.Line,
				Block:    blockIndex,
			})
			instrumentRuntimeBlock(function, block, id)
		}
		if found && entryID >= 0 {
			coverage.Functions[metadataIndex].Entry = entryID
		}
	}

	module.Data = append(module.Data, &ir.Data{
		Name:  runtimeCoverageHeaderData,
		Align: 8,
		Items: []ir.DataItem{{Sub: ir.SubUB, Str: string(runtimeCoverageHeader)}},
	})
	for offset := 0; offset < len(coverage.Blocks); offset += runtimeCoverageChunkSize {
		size := min(runtimeCoverageChunkSize, len(coverage.Blocks)-offset)
		module.Data = append(module.Data, &ir.Data{
			Name:  runtimeCoverageChunkName(offset / runtimeCoverageChunkSize),
			Align: 8,
			Items: []ir.DataItem{{Sub: ir.SubUB, Zero: size}},
		})
	}
	module.Data = append(module.Data, &ir.Data{
		Name:  runtimeCoverageFooterData,
		Align: 8,
		Items: []ir.DataItem{{Sub: ir.SubUB, Str: string(runtimeCoverageFooter)}},
	})
	addRuntimeCoverageDump(module, len(coverage.Blocks))
	if !insertRuntimeCoverageDumpCalls(module) {
		return fmt.Errorf("goc: runtime coverage could not find a call to runtime.exit")
	}
	return nil
}

func activeRuntimeSourceID(unit *sourceUnit) (string, error) {
	type runtimeSourceFile struct {
		name    string
		content []byte
	}

	files := make([]runtimeSourceFile, 0, len(unit.files))
	for _, sourceFile := range unit.files {
		position := unitPosition(unit, sourceFile.Pos())
		content, err := os.ReadFile(position.Filename)
		if err != nil {
			return "", fmt.Errorf("goc: hash active runtime source %s: %w", position.Filename, err)
		}
		files = append(files, runtimeSourceFile{
			name:    runtimeCoverageFile(position.Filename),
			content: content,
		})
	}
	sort.Slice(files, func(left, right int) bool {
		return files[left].name < files[right].name
	})

	digest := sha256.New()
	for _, file := range files {
		digest.Write([]byte(file.name))
		digest.Write([]byte{0})
		digest.Write(file.content)
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func activeRuntimeFunctions(
	unit *sourceUnit,
	linkNames map[*types.Func]string,
	initSymbols map[*types.Func]string,
) []RuntimeCoverageFunction {
	var functions []RuntimeCoverageFunction
	for _, sourceFile := range unit.files {
		for _, declaration := range sourceFile.Decls {
			functionDeclaration, ok := declaration.(*ast.FuncDecl)
			if !ok || functionDeclaration.Body == nil {
				continue
			}
			function, ok := unit.info.Defs[functionDeclaration.Name].(*types.Func)
			if !ok {
				continue
			}
			name := functionSymbol(function)
			if linkNames[function] != "" {
				name = linkNames[function]
			}
			if initSymbols[function] != "" {
				name = initSymbols[function]
			}
			position := unitPosition(unit, functionDeclaration.Pos())
			functions = append(functions, RuntimeCoverageFunction{
				Name:  name,
				File:  runtimeCoverageFile(position.Filename),
				Line:  position.Line,
				Entry: -1,
			})
		}
	}
	sort.Slice(functions, func(left, right int) bool {
		if functions[left].File != functions[right].File {
			return functions[left].File < functions[right].File
		}
		if functions[left].Line != functions[right].Line {
			return functions[left].Line < functions[right].Line
		}
		return functions[left].Name < functions[right].Name
	})
	return functions
}

func unitPosition(unit *sourceUnit, position token.Pos) token.Position {
	return unit.fset.Position(position)
}

func runtimeFunctionPosition(module *ir.Module, function *ir.Func, activeFiles map[string]bool) (token.Position, bool) {
	for _, block := range function.Blocks {
		position, ok := runtimeIRPosition(module, block.Pos, activeFiles)
		if ok {
			return position, true
		}
		for _, instruction := range block.Instrs {
			position, ok := runtimeIRPosition(module, instruction.Pos, activeFiles)
			if ok {
				return position, true
			}
		}
	}
	return token.Position{}, false
}

func runtimeBlockPosition(module *ir.Module, block *ir.Block, fallback token.Position, activeFiles map[string]bool) token.Position {
	if position, ok := runtimeIRPosition(module, block.Pos, activeFiles); ok {
		return position
	}
	for _, instruction := range block.Instrs {
		if position, ok := runtimeIRPosition(module, instruction.Pos, activeFiles); ok {
			return position
		}
	}
	return fallback
}

func runtimeIRPosition(module *ir.Module, position ir.SrcPos, activeFiles map[string]bool) (token.Position, bool) {
	if !position.Valid() {
		return token.Position{}, false
	}
	filename := module.FileName(position.File)
	if !activeFiles[canonicalCoveragePath(filename)] {
		return token.Position{}, false
	}
	return token.Position{Filename: filename, Line: int(position.Line), Column: int(position.Col)}, true
}

func canonicalCoveragePath(filename string) string {
	if filename == "" {
		return ""
	}
	// Names taken from the module's file table are repository-relative (see
	// goc/trimpath.go), and resolving those against the process's working
	// directory would name a file that does not exist. Names taken from the
	// fset are absolute already and UntrimPath leaves them alone.
	filename = UntrimPath(filename)
	absolute, err := filepath.Abs(filename)
	if err == nil {
		filename = absolute
	}
	return filepath.Clean(filename)
}

func runtimeCoverageFile(filename string) string {
	filename = filepath.ToSlash(filename)
	if marker := strings.LastIndex(filename, "/runtime/"); marker >= 0 {
		return filename[marker+1:]
	}
	return filepath.Base(filename)
}

func instrumentRuntimeBlock(function *ir.Func, block *ir.Block, id int) {
	value := function.Word(1)
	chunk := id / runtimeCoverageChunkSize
	offset := id % runtimeCoverageChunkSize
	address := function.Sym(runtimeCoverageChunkName(chunk), int64(offset))
	instruction := ir.Instr{
		Op:   ir.OStoreb,
		Cls:  ir.ClsW,
		Args: []ir.Ref{value, address},
		Pos:  block.Instrs[0].Pos,
	}
	block.Instrs = append([]ir.Instr{instruction}, block.Instrs...)
}

func addRuntimeCoverageDump(module *ir.Module, size int) {
	function := module.NewFuncVoid(runtimeCoverageDump)
	function.NoSplit = true
	function.CallConv = ir.CallConvPlatform
	function.ManagedFrame = false
	block := function.Entry()
	addRuntimeCoverageWrite(function, block, function.Sym(runtimeCoverageHeaderData, 0), len(runtimeCoverageHeader))
	for offset := 0; offset < size; offset += runtimeCoverageChunkSize {
		chunkSize := min(runtimeCoverageChunkSize, size-offset)
		chunk := offset / runtimeCoverageChunkSize
		addRuntimeCoverageWrite(function, block, function.Sym(runtimeCoverageChunkName(chunk), 0), chunkSize)
	}
	addRuntimeCoverageWrite(function, block, function.Sym(runtimeCoverageFooterData, 0), len(runtimeCoverageFooter))
	block.RetVoid()
}

func addRuntimeCoverageWrite(function *ir.Func, block *ir.Block, address ir.Ref, size int) {
	block.Asm("mov x1, %x0\nmov x2, %x1\nmov x0, #2\nmov x8, #64\nsvc #0", []ir.AsmSpec{
		{Kind: ir.AsmRegIn, Ref: address},
		{Kind: ir.AsmRegIn, Ref: function.Long(int64(size))},
	})
	assembly := &block.Instrs[len(block.Instrs)-1]
	assembly.Asm.ExactClobbers = true
	assembly.Asm.Clobbers = []string{"x0", "x1", "x2", "x8", "memory", "cc"}
}

func runtimeCoverageChunkName(chunk int) string {
	return fmt.Sprintf(".goc.runtime.coverage.%d", chunk)
}

func insertRuntimeCoverageDumpCalls(module *ir.Module) bool {
	inserted := false
	for _, function := range module.Funcs {
		if function.Name == runtimeCoverageDump {
			continue
		}
		for _, block := range function.Blocks {
			for index := 0; index < len(block.Instrs); index++ {
				instruction := block.Instrs[index]
				if instruction.Op != ir.OCall || len(instruction.Args) == 0 {
					continue
				}
				callee := instruction.Args[0]
				if callee.Kind != ir.RefConst {
					continue
				}
				constant := function.Consts[callee.ID]
				if constant.Kind != ir.ConstSym || constant.Sym != "runtime.exit" {
					continue
				}

				block.CallVoidWithConvention(ir.CallConvPlatform, function.Sym(runtimeCoverageDump, 0))
				dumpCall := block.Instrs[len(block.Instrs)-1]
				block.Instrs = block.Instrs[:len(block.Instrs)-1]
				block.Instrs = append(block.Instrs, ir.Instr{})
				copy(block.Instrs[index+1:], block.Instrs[index:])
				block.Instrs[index] = dumpCall
				index++
				inserted = true
			}
		}
	}
	return inserted
}
