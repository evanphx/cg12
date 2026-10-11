package plan9asm

import (
	"bufio"
	"bytes"
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// amd64DataSymbol is one data symbol's size, contents, and relocations.
type amd64DataSymbol struct {
	size        int
	content     []byte
	relocations map[int]amd64DataRelocation
}

type amd64DataRelocation struct {
	width  int
	target string
	addend int64
}

var goDataHeader = regexp.MustCompile(`^(\S+) (S(?:NOPTR)?(?:RO)?DATA(?:FIPS)?|S(?:NOPTR)?BSS)\b.*\bsize=(\d+)`)
var goDataBytes = regexp.MustCompile(`^\t0x([0-9a-f]+) ((?:[0-9a-f]{2} )+)`)
var goDataRelocation = regexp.MustCompile(`^\trel (\d+)\+(\d+) t=(\S+) (\S+)\+(-?\d+)`)
var goAnySymbolHeader = regexp.MustCompile(`^\S+ S[A-Z]+ `)

// goDataSymbols lists the data symbols `go tool asm -S` reports for a file,
// keyed by the name the translator gives them.
func goDataSymbols(t *testing.T, goTool string, file amd64SourceFile, fileTag string) map[string]*amd64DataSymbol {
	t.Helper()
	directory := t.TempDir()
	var header strings.Builder
	defines := amd64AssemblyDefines(t, file.packagePath)
	for _, name := range sortedKeys(defines) {
		fmt.Fprintf(&header, "#define %s %d\n", name, defines[name])
	}
	require.NoError(t, os.WriteFile(filepath.Join(directory, "go_asm.h"), []byte(header.String()), 0o644))
	source, err := filepath.Abs(filepath.Join("..", "stdlib", "src", filepath.FromSlash(file.packagePath), file.name))
	require.NoError(t, err)
	runtimeDirectory, err := filepath.Abs(filepath.Join("..", "stdlib", "src", "runtime"))
	require.NoError(t, err)
	command := exec.Command(goTool, "tool", "asm", "-S", "-p", file.packagePath,
		"-I", directory, "-I", runtimeDirectory,
		"-D", "GOOS_linux", "-D", "GOARCH_amd64", "-D", "GOAMD64_v1",
		"-o", filepath.Join(directory, "go.o"), source)
	command.Env = append(os.Environ(), "GOARCH=amd64", "GOOS=linux", "GOAMD64=v1")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)

	symbols := make(map[string]*amd64DataSymbol)
	var current *amd64DataSymbol
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if match := goDataHeader.FindStringSubmatch(line); match != nil {
			name := match[1]
			current = nil
			// Assembler-generated symbols (stack maps, float constants) are
			// cg12's or the translation's own business.
			if strings.HasPrefix(name, "gclocals") || strings.HasPrefix(name, "$f") || strings.HasPrefix(name, "go:") {
				continue
			}
			size, _ := strconv.Atoi(match[3])
			current = &amd64DataSymbol{size: size, content: make([]byte, size), relocations: make(map[int]amd64DataRelocation)}
			if strings.Contains(line, " static ") {
				name = ".L" + fileTag + "_" + sanitizeSymbol(name)
			} else {
				name = sanitizeSymbol(name)
			}
			symbols[name] = current
			continue
		}
		if goAnySymbolHeader.MatchString(line) {
			current = nil
			continue
		}
		if current == nil {
			continue
		}
		if match := goDataBytes.FindStringSubmatch(line); match != nil {
			offset, _ := strconv.ParseUint(match[1], 16, 64)
			code, err := hexBytes(strings.ReplaceAll(match[2], " ", ""))
			require.NoError(t, err)
			copy(current.content[offset:], code)
			continue
		}
		if match := goDataRelocation.FindStringSubmatch(line); match != nil {
			offset, _ := strconv.Atoi(match[1])
			width, _ := strconv.Atoi(match[2])
			addend, _ := strconv.ParseInt(match[5], 10, 64)
			current.relocations[offset] = amd64DataRelocation{width: width, target: match[4], addend: addend}
		}
	}
	require.NoError(t, scanner.Err())
	return symbols
}

// elfDataSymbols reads the data symbols of an assembled translation.
func elfDataSymbols(t *testing.T, object string) map[string]*amd64DataSymbol {
	t.Helper()
	file, err := elf.Open(object)
	require.NoError(t, err)
	defer file.Close()
	elfSymbols, err := file.Symbols()
	require.NoError(t, err)
	symbols := make(map[string]*amd64DataSymbol)
	sectionRelocations := make(map[int][]elf.Rela64)
	for _, section := range file.Sections {
		if section.Type != elf.SHT_RELA {
			continue
		}
		data, err := section.Data()
		require.NoError(t, err)
		var relocations []elf.Rela64
		for offset := 0; offset+24 <= len(data); offset += 24 {
			relocations = append(relocations, elf.Rela64{
				Off:    file.ByteOrder.Uint64(data[offset:]),
				Info:   file.ByteOrder.Uint64(data[offset+8:]),
				Addend: int64(file.ByteOrder.Uint64(data[offset+16:])),
			})
		}
		sectionRelocations[int(section.Info)] = relocations
	}
	for _, symbol := range elfSymbols {
		if elf.ST_TYPE(symbol.Info) != elf.STT_OBJECT || symbol.Section >= elf.SHN_LORESERVE {
			continue
		}
		section := file.Sections[symbol.Section]
		content := make([]byte, symbol.Size)
		if section.Type != elf.SHT_NOBITS {
			data, err := section.Data()
			require.NoError(t, err)
			copy(content, data[symbol.Value:symbol.Value+symbol.Size])
		}
		result := &amd64DataSymbol{size: int(symbol.Size), content: content, relocations: make(map[int]amd64DataRelocation)}
		for _, relocation := range sectionRelocations[int(symbol.Section)] {
			if relocation.Off < symbol.Value || relocation.Off >= symbol.Value+symbol.Size {
				continue
			}
			target := elfSymbols[elf.R_SYM64(relocation.Info)-1]
			width := 8
			if elf.R_X86_64(elf.R_TYPE64(relocation.Info)) == elf.R_X86_64_32 || elf.R_X86_64(elf.R_TYPE64(relocation.Info)) == elf.R_X86_64_32S {
				width = 4
			}
			name := target.Name
			addend := relocation.Addend
			if elf.ST_TYPE(target.Info) == elf.STT_SECTION {
				name = "section:" + file.Sections[target.Section].Name
			}
			result.relocations[int(relocation.Off-symbol.Value)] = amd64DataRelocation{width: width, target: name, addend: addend}
		}
		symbols[symbol.Name] = result
	}
	return symbols
}

// TestAMD64DataMatchesGoAssembler compares every DATA/GLOBL symbol of every
// allowlisted file with what `go tool asm` produces: size, contents, and the
// position, width, target, and addend of each relocation.
func TestAMD64DataMatchesGoAssembler(t *testing.T) {
	cc := requireAMD64Assembler(t)
	goTool := requireMatchingGo(t)
	compared := 0
	for _, file := range amd64VerifiedFiles() {
		t.Run(file.packagePath+"/"+file.name, func(t *testing.T) {
			fileTag := sanitizeSymbol(file.packagePath + "_" + strings.TrimSuffix(file.name, ".s"))
			want := goDataSymbols(t, goTool, file, fileTag)
			if len(want) == 0 {
				return
			}
			translation := translateAMD64SourceFile(t, file)
			// File-local symbols are .L names, which GNU as keeps out of the
			// symbol table; drop the dot so they can be found by name.
			visible := strings.ReplaceAll(translation.Assembly, ".L"+fileTag+"_", "L"+fileTag+"_")
			got := elfDataSymbols(t, assembleAMD64(t, cc, "data", visible))
			for name, symbol := range want {
				mine := got[strings.TrimPrefix(name, ".")]
				require.NotNil(t, mine, "translation does not define %s", name)
				compared++
				require.Equal(t, symbol.size, mine.size, "%s size", name)
				for offset, relocation := range symbol.relocations {
					other, ok := mine.relocations[offset]
					require.True(t, ok, "%s: no relocation at %d", name, offset)
					require.Equal(t, relocation.width, other.width, "%s+%d relocation width", name, offset)
					require.Equal(t, relocation.addend, other.addend, "%s+%d relocation addend", name, offset)
					// -S prints a file-local target without its <> marker.
					target := sanitizeSymbol(relocation.target)
					candidates := []string{target, abi0Symbol(target), "L" + fileTag + "_" + target}
					require.Contains(t, candidates, other.target, "%s+%d relocation target", name, offset)
					for index := offset; index < offset+relocation.width; index++ {
						mine.content[index] = 0
					}
				}
				require.Len(t, mine.relocations, len(symbol.relocations), "%s relocations", name)
				require.Equal(t, symbol.content, mine.content, "%s contents", name)
			}
			t.Logf("%d data symbols: %v", len(want), sortedKeys(want))
		})
	}
	t.Logf("compared %d data symbols", compared)
}
