package plan9asm

import (
	"bufio"
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

// The Go toolchain's own encoder tests (cmd/asm/internal/asm/testdata
// amd64enc.s, amd64enc_extra.s, avx512enc/*.s) pair about 25,000 Plan 9
// amd64 instructions with the bytes cmd/asm must produce for them. This test
// translates each instruction the amd64 translator accepts, assembles the
// result with the system assembler, and requires either the same bytes or,
// where GNU as legitimately picks another encoding, the same decoded
// instruction. Instructions the translator rejects are counted, not failed:
// the translator only has to cover what the runtime and library assembly
// uses, and the allowlist tests hold it to that.

type amd64CorpusLine struct {
	file     string
	line     int
	source   string
	expected []string
}

var amd64CorpusPattern = regexp.MustCompile(`^\s+([A-Z][A-Z0-9_.]*(?:\s+[^/]*?)?)\s*//\s*([0-9a-f]+(?:\s+or\s+[0-9a-f]+)*)\s*$`)

func readAMD64Corpus(t *testing.T, goroot string) []amd64CorpusLine {
	t.Helper()
	directory := filepath.Join(goroot, "src", "cmd", "asm", "internal", "asm", "testdata")
	files := []string{filepath.Join(directory, "amd64enc.s"), filepath.Join(directory, "amd64enc_extra.s")}
	extra, err := filepath.Glob(filepath.Join(directory, "avx512enc", "*.s"))
	require.NoError(t, err)
	files = append(files, extra...)
	var lines []amd64CorpusLine
	for _, path := range files {
		handle, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		scanner := bufio.NewScanner(handle)
		number := 0
		for scanner.Scan() {
			number++
			match := amd64CorpusPattern.FindStringSubmatch(scanner.Text())
			if match == nil {
				continue
			}
			lines = append(lines, amd64CorpusLine{
				file:     filepath.Base(path),
				line:     number,
				source:   strings.TrimSpace(match[1]),
				expected: strings.Split(strings.ReplaceAll(match[2], " or ", " "), " "),
			})
		}
		require.NoError(t, scanner.Err())
		require.NoError(t, handle.Close())
	}
	return lines
}

func translateAMD64CorpusLine(index int, source string) (string, error) {
	text := fmt.Sprintf("TEXT ·corpus%d(SB),NOSPLIT|NOFRAME,$0\n\t%s\n", index, source)
	file, err := ParseWithOptions(strings.NewReader(text), ParseOptions{Arch: "amd64"})
	if err != nil {
		return "", err
	}
	return TranslateAMD64(file, AMD64Options{PackagePath: "corpus", Filename: "corpus.s"})
}

var gasErrorLine = regexp.MustCompile(`\.S:(\d+): Error`)

func TestAMD64InstructionEncodingCorpus(t *testing.T) {
	cc := requireAMD64Assembler(t)
	goTool := requireMatchingGo(t)
	objdump := requireTool(t, "llvm-objdump")
	output, err := exec.Command(goTool, "env", "GOROOT").Output()
	if err != nil {
		t.Skip("go env GOROOT failed")
	}
	corpus := readAMD64Corpus(t, strings.TrimSpace(string(output)))
	if len(corpus) == 0 {
		t.Skip("the Go encoder corpus is unavailable")
	}

	unsupported := make(map[string]int)
	translated := make(map[int]string)
	for index, line := range corpus {
		assembly, err := translateAMD64CorpusLine(index, line.source)
		if err != nil {
			opcode := strings.Fields(line.source)[0]
			unsupported[opcode]++
			continue
		}
		translated[index] = assembly
	}

	// Assemble everything at once; an instruction GNU as rejects is a
	// translation bug, so it is reported and dropped before reassembling.
	var failures []string
	var object string
	for attempt := 0; attempt < 50; attempt++ {
		var source strings.Builder
		lineOwner := make(map[int]int)
		lineNumber := 1
		for _, index := range sortedIntKeys(translated) {
			assembly := translated[index]
			for range strings.Count(assembly, "\n") {
				lineOwner[lineNumber] = index
				lineNumber++
			}
			source.WriteString(assembly)
		}
		directory := t.TempDir()
		sourcePath := filepath.Join(directory, "corpus.S")
		object = filepath.Join(directory, "corpus.o")
		require.NoError(t, os.WriteFile(sourcePath, []byte(source.String()), 0o644))
		output, err := exec.Command(cc, "-c", "-o", object, sourcePath).CombinedOutput()
		if err == nil {
			break
		}
		matches := gasErrorLine.FindAllStringSubmatch(string(output), -1)
		require.NotEmpty(t, matches, "%s", output)
		for _, match := range matches {
			number, _ := strconv.Atoi(match[1])
			index, ok := lineOwner[number]
			if !ok {
				continue
			}
			if _, present := translated[index]; !present {
				continue
			}
			failures = append(failures, fmt.Sprintf("%s:%d: %s: GNU as rejected the translation %q", corpus[index].file, corpus[index].line, corpus[index].source, amd64CorpusBody(translated[index])))
			delete(translated, index)
		}
	}

	decoded := disassembleAMD64(t, objdump, object)
	references := make(map[string][]byte)
	var order []string
	mismatched := make(map[int][]amd64Instruction)
	matched := 0
	for _, index := range sortedIntKeys(translated) {
		name := abi0Symbol(fmt.Sprintf("corpus_corpus%d", index))
		instructions := decoded[name]
		code := ""
		for _, instruction := range instructions {
			code += instruction.code
		}
		expected := corpus[index].expected
		if containsString(expected, code) {
			matched++
			continue
		}
		reference, err := hexBytes(expected[0])
		require.NoError(t, err)
		key := strconv.Itoa(index)
		references[key] = reference
		order = append(order, key)
		mismatched[index] = instructions
	}
	equivalent := 0
	if len(order) > 0 {
		decodedReferences := decodeAMD64Bytes(t, cc, objdump, references, nil, order)
		for index, instructions := range mismatched {
			want := normalizeAMD64(decodedReferences[strconv.Itoa(index)])
			got := normalizeAMD64(instructions)
			same := len(want) == len(got)
			for position := 0; same && position < len(want); position++ {
				same = equivalentAMD64(want[position], got[position])
			}
			if same {
				equivalent++
				continue
			}
			failures = append(failures, fmt.Sprintf("%s:%d: %s\n\tgo:  %v (%s)\n\tgas: %v", corpus[index].file, corpus[index].line, corpus[index].source, want, strings.Join(corpus[index].expected, " or "), got))
		}
	}

	total := 0
	for _, count := range unsupported {
		total += count
	}
	t.Logf("corpus: %d instructions; %d translated (%d identical bytes, %d equivalent encodings); %d rejected by the translator (%d mnemonics)",
		len(corpus), len(translated), matched, equivalent, total, len(unsupported))
	if os.Getenv("PLAN9ASM_AMD64_CORPUS_VERBOSE") != "" {
		for _, opcode := range sortedKeys(unsupported) {
			t.Logf("rejected: %s (%d)", opcode, unsupported[opcode])
		}
	}
	sort.Strings(failures)
	for _, failure := range failures {
		t.Error(failure)
	}
}

// amd64CorpusBody returns the instructions of a one-instruction translation.
func amd64CorpusBody(assembly string) string {
	var body []string
	for _, line := range strings.Split(assembly, "\n") {
		if strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "\t.") {
			body = append(body, strings.TrimSpace(line))
		}
	}
	return strings.Join(body, "; ")
}

func sortedIntKeys[V any](values map[int]V) []int {
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
