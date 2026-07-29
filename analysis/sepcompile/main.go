// Command sepcompile measures how much of the compiled Go runtime is invariant
// across the programs it is linked into. It is throwaway instrumentation for the
// separate-compilation feasibility spike: it changes no compiler behaviour, it
// only compiles each named program the way `goc` does and reports the resulting
// symbols.
//
// Two identities are reported per symbol.
//
//   - Hash is content identity under the symbol's own name: the symbol's bytes
//     with every relocated field blanked out, plus the relocations that patch
//     them, each recorded by its target's *name*. Two symbols with equal Hash are
//     interchangeable exactly as they stand.
//
//   - Colour is content identity modulo a consistent renaming of the symbols cg12
//     names by a running counter. Those names (.goc.string.17, .goc.itab.4) are
//     positions in the module being built, so adding user code renumbers runtime
//     symbols that did not otherwise change; Hash counts every such renumbering as
//     a difference. Colour is a Weisfeiler-Lehman refinement over the relocation
//     graph in which a counter-named symbol contributes its colour and every other
//     symbol contributes its name, so a reference to `runtime.mallocgc` still has
//     to be a reference to `runtime.mallocgc`, while a reference to whichever
//     string literal happens to hold "out of memory" does not have to be to the
//     same numbered symbol.
//
// Hash is what a linker could share as it stands. Colour is what a linker could
// share if cg12 named those symbols by their content instead of by a counter.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/evanphx/cg12/arm64"
	"github.com/evanphx/cg12/goc"
	"github.com/evanphx/cg12/ir"
	"github.com/evanphx/cg12/obj"
)

// refinementRounds is how many times a symbol's colour absorbs its neighbours'.
// Each round widens the neighbourhood the colour summarizes by one call edge.
const refinementRounds = 6

// symbolRecord is one defined symbol of a compiled program.
type symbolRecord struct {
	Name      string `json:"n"`
	Section   string `json:"s"`
	Value     uint64 `json:"v"`
	Size      uint64 `json:"z"`
	Global    bool   `json:"g"`
	Func      bool   `json:"f"`
	Counter   bool   `json:"a"`
	Hash      string `json:"h"`
	Colour    string `json:"c"`
	Relocs    int    `json:"r"`
	NoContent bool   `json:"e"`
}

// counterNamed reports whether a symbol's name encodes its position in the
// module being compiled rather than anything about the symbol. cg12 builds these
// names from a running count (len(mod.Data), len(interfaceItabs), ...), so user
// code that emits one more datum renumbers every later runtime symbol without
// changing it. The families are taken from the Sprintf sites in goc/compile.go;
// the names here are the sanitized form the object carries, with dots as
// underscores.
var counterNamed = []*regexp.Regexp{
	regexp.MustCompile(`^_goc_string_\d+`),
	regexp.MustCompile(`^_goc_cstring_\d+`),
	regexp.MustCompile(`^_goc_bytes_\d+`),
	regexp.MustCompile(`^_goc_runtime_type_\d+`),
	regexp.MustCompile(`^_goc_channel_element_\d+`),
	regexp.MustCompile(`^_goc_itab_\d+`),
	regexp.MustCompile(`^_goc_goabi_\d+`),
	regexp.MustCompile(`^_goc_module_inittask_\d+`),
	regexp.MustCompile(`^_goc_global_init_\d+`),
	regexp.MustCompile(`^_goc_global_initfunc_\d+`),
	regexp.MustCompile(`^_goc_global_literal_\d+`),
	regexp.MustCompile(`_interfacecall_\d+`),
	regexp.MustCompile(`_interfacecall_promoted_\d+`),
	regexp.MustCompile(`_gointernal_funcvalue_\d+`),
}

func isCounterNamed(name string) bool {
	// A funcval wrapper inherits its target's name, so it is counter-named
	// exactly when its target is.
	name = strings.TrimPrefix(name, "_goc_funcval_")
	for _, pattern := range counterNamed {
		if pattern.MatchString(name) {
			return true
		}
	}
	return false
}

type programRecord struct {
	Program   string         `json:"program"`
	TextSize  int            `json:"text_size"`
	DataSize  int            `json:"data_size"`
	RodataN   int            `json:"rodata_size"`
	RelroN    int            `json:"relro_size"`
	BssSize   int            `json:"bss_size"`
	Undefined []string       `json:"undefined"`
	Symbols   []symbolRecord `json:"symbols"`
	RelocKind map[string]int `json:"reloc_kinds"`
}

func sectionName(k obj.SecKind) string {
	switch k {
	case obj.SecUndef:
		return "undef"
	case obj.SecText:
		return "text"
	case obj.SecData:
		return "data"
	case obj.SecRodata:
		return "rodata"
	case obj.SecRelro:
		return "relro"
	case obj.SecStackMap:
		return "stackmap"
	case obj.SecTdata:
		return "tdata"
	case obj.SecBss:
		return "bss"
	case obj.SecTbss:
		return "tbss"
	}
	return fmt.Sprintf("sec%d", int(k))
}

func relocName(t uint32) string {
	switch t {
	case obj.R_AARCH64_CALL26:
		return "CALL26"
	case obj.R_AARCH64_JUMP26:
		return "JUMP26"
	case obj.R_AARCH64_ADR_PREL_PG_HI21:
		return "ADR_PREL_PG_HI21"
	case obj.R_AARCH64_ADR_PREL_LO21:
		return "ADR_PREL_LO21"
	case obj.R_AARCH64_ADD_ABS_LO12_NC:
		return "ADD_ABS_LO12_NC"
	case obj.R_AARCH64_ABS64:
		return "ABS64"
	case obj.R_AARCH64_ABS32:
		return "ABS32"
	case obj.R_AARCH64_LDST8_ABS_LO12_NC:
		return "LDST8_ABS_LO12_NC"
	case obj.R_AARCH64_LDST16_ABS_LO12_NC:
		return "LDST16_ABS_LO12_NC"
	case obj.R_AARCH64_LDST32_ABS_LO12_NC:
		return "LDST32_ABS_LO12_NC"
	case obj.R_AARCH64_LDST64_ABS_LO12_NC:
		return "LDST64_ABS_LO12_NC"
	case obj.R_AARCH64_LDST128_ABS_LO12_NC:
		return "LDST128_ABS_LO12_NC"
	}
	return fmt.Sprintf("R%d", t)
}

// maskReloc zeroes the bits a relocation of this type writes, so two copies of
// the same code that reference the same symbol hash identically no matter where
// either copy or its target landed.
func maskReloc(section []byte, r obj.Reloc) {
	off := int(r.Offset)
	switch r.Type {
	case obj.R_AARCH64_CALL26, obj.R_AARCH64_JUMP26:
		if off+4 > len(section) {
			return
		}
		word := binary.LittleEndian.Uint32(section[off:])
		binary.LittleEndian.PutUint32(section[off:], word&0xfc000000)
	case obj.R_AARCH64_ADR_PREL_PG_HI21, obj.R_AARCH64_ADR_PREL_LO21:
		if off+4 > len(section) {
			return
		}
		word := binary.LittleEndian.Uint32(section[off:])
		// immlo is bits 29-30, immhi bits 5-23.
		binary.LittleEndian.PutUint32(section[off:], word&^uint32(0x60ffffe0))
	case obj.R_AARCH64_ADD_ABS_LO12_NC,
		obj.R_AARCH64_LDST8_ABS_LO12_NC,
		obj.R_AARCH64_LDST16_ABS_LO12_NC,
		obj.R_AARCH64_LDST32_ABS_LO12_NC,
		obj.R_AARCH64_LDST64_ABS_LO12_NC,
		obj.R_AARCH64_LDST128_ABS_LO12_NC:
		if off+4 > len(section) {
			return
		}
		word := binary.LittleEndian.Uint32(section[off:])
		// imm12 is bits 10-21.
		binary.LittleEndian.PutUint32(section[off:], word&^uint32(0x003ffc00))
	case obj.R_AARCH64_ABS64:
		if off+8 > len(section) {
			return
		}
		clear(section[off : off+8])
	case obj.R_AARCH64_ABS32:
		if off+4 > len(section) {
			return
		}
		clear(section[off : off+4])
	default:
		// Unknown: blank four bytes and let the reloc-kind census show it.
		if off+4 > len(section) {
			return
		}
		clear(section[off : off+4])
	}
}

func sectionBytes(o *obj.Object, k obj.SecKind) []byte {
	switch k {
	case obj.SecText:
		return o.Text
	case obj.SecData:
		return o.Data
	case obj.SecRodata:
		return o.Rodata
	case obj.SecRelro:
		return o.Relro
	}
	return nil
}

func sectionRelocs(o *obj.Object, k obj.SecKind) []obj.Reloc {
	switch k {
	case obj.SecText:
		return o.Relocs
	case obj.SecData:
		return o.DataRelocs
	case obj.SecRodata:
		return o.RodataRelocs
	case obj.SecRelro:
		return o.RelroRelocs
	}
	return nil
}

// edge is one relocation inside a symbol, with its offset made relative to the
// symbol's own start so the symbol's position is not part of its identity.
type edge struct {
	offset uint64
	kind   uint32
	addend int64
	target int    // index of the defined symbol referenced, or -1
	extern string // referenced name, when the target is not defined here
}

// node is a defined symbol together with everything its identity is computed from.
type node struct {
	record symbolRecord
	body   []byte
	edges  []edge
}

var contentSections = []obj.SecKind{obj.SecText, obj.SecData, obj.SecRodata, obj.SecRelro}

func analyze(program string, o *obj.Object) programRecord {
	record := programRecord{
		Program:   program,
		TextSize:  len(o.Text),
		DataSize:  len(o.Data),
		RodataN:   len(o.Rodata),
		RelroN:    len(o.Relro),
		BssSize:   o.BssSize,
		RelocKind: map[string]int{},
	}
	// Masked copies of every section, so a symbol's bytes can be hashed with the
	// relocated fields blanked out.
	masked := map[obj.SecKind][]byte{}
	for _, kind := range contentSections {
		copyOf := append([]byte(nil), sectionBytes(o, kind)...)
		for _, relocation := range sectionRelocs(o, kind) {
			maskReloc(copyOf, relocation)
			record.RelocKind[relocName(relocation.Type)]++
		}
		masked[kind] = copyOf
	}
	// Relocations sorted by offset per section, so each symbol's slice is found
	// by binary search rather than a scan of the whole list.
	sortedRelocs := map[obj.SecKind][]obj.Reloc{}
	for _, kind := range contentSections {
		list := append([]obj.Reloc(nil), sectionRelocs(o, kind)...)
		sort.Slice(list, func(i, j int) bool { return list[i].Offset < list[j].Offset })
		sortedRelocs[kind] = list
	}

	index := make(map[string]int, len(o.Syms))
	nodes := make([]node, 0, len(o.Syms))
	for _, symbol := range o.Syms {
		entry := symbolRecord{
			Name:    symbol.Name,
			Section: sectionName(symbol.Section),
			Value:   symbol.Value,
			Size:    symbol.Size,
			Global:  symbol.Global,
			Func:    symbol.Func,
			Counter: isCounterNamed(symbol.Name),
		}
		current := node{record: entry}
		body := masked[symbol.Section]
		if body == nil || symbol.Size == 0 {
			current.record.NoContent = true
		}
		if body != nil && symbol.Size > 0 {
			start, end := int(symbol.Value), int(symbol.Value+symbol.Size)
			if end > len(body) {
				end = len(body)
			}
			if start > end {
				start = end
			}
			current.body = body[start:end]
			list := sortedRelocs[symbol.Section]
			low := sort.Search(len(list), func(i int) bool { return list[i].Offset >= uint64(start) })
			for i := low; i < len(list) && list[i].Offset < uint64(end); i++ {
				current.edges = append(current.edges, edge{
					offset: list[i].Offset - uint64(start),
					kind:   list[i].Type,
					addend: list[i].Addend,
					target: -1,
					extern: list[i].Sym,
				})
			}
		}
		current.record.Relocs = len(current.edges)
		if _, seen := index[symbol.Name]; !seen {
			index[symbol.Name] = len(nodes)
		}
		nodes = append(nodes, current)
	}
	undefined := map[string]bool{}
	for i := range nodes {
		for j := range nodes[i].edges {
			target, ok := index[nodes[i].edges[j].extern]
			if ok {
				nodes[i].edges[j].target = target
				nodes[i].edges[j].extern = ""
				continue
			}
			undefined[nodes[i].edges[j].extern] = true
		}
	}
	for name := range undefined {
		record.Undefined = append(record.Undefined, name)
	}
	sort.Strings(record.Undefined)

	assignNameHashes(nodes)
	assignColours(nodes)
	record.Symbols = make([]symbolRecord, len(nodes))
	for i := range nodes {
		record.Symbols[i] = nodes[i].record
	}
	return record
}

// assignNameHashes computes the name-carrying content identity: the symbol's
// masked bytes plus each relocation's offset, kind, addend and target *name*.
func assignNameHashes(nodes []node) {
	names := make([]string, len(nodes))
	for i := range nodes {
		names[i] = nodes[i].record.Name
	}
	for i := range nodes {
		hasher := sha256.New()
		fmt.Fprintf(hasher, "%s|%d|", nodes[i].record.Section, nodes[i].record.Size)
		hasher.Write(nodes[i].body)
		for _, e := range nodes[i].edges {
			target := e.extern
			if e.target >= 0 {
				target = names[e.target]
			}
			fmt.Fprintf(hasher, "\x00%d|%d|%d|%s", e.offset, e.kind, e.addend, target)
		}
		nodes[i].record.Hash = hex.EncodeToString(hasher.Sum(nil)[:12])
	}
}

// assignColours computes the identity that is invariant under renumbering the
// counter-named symbols. A reference to a stably named symbol contributes that
// name; a reference to a counter-named one contributes the referent's colour, so
// only the counter-named subgraph needs refining.
func assignColours(nodes []node) {
	names := make([]string, len(nodes))
	counter := make([]bool, len(nodes))
	for i := range nodes {
		names[i] = nodes[i].record.Name
		counter[i] = nodes[i].record.Counter
	}
	colours := make([][]byte, len(nodes))
	for i := range nodes {
		hasher := sha256.New()
		fmt.Fprintf(hasher, "%s|%d|", nodes[i].record.Section, nodes[i].record.Size)
		hasher.Write(nodes[i].body)
		for _, e := range nodes[i].edges {
			if e.target >= 0 && counter[e.target] {
				fmt.Fprintf(hasher, "\x00%d|%d|%d|#", e.offset, e.kind, e.addend)
				continue
			}
			target := e.extern
			if e.target >= 0 {
				target = names[e.target]
			}
			fmt.Fprintf(hasher, "\x00%d|%d|%d|%s", e.offset, e.kind, e.addend, target)
		}
		colours[i] = hasher.Sum(nil)[:16]
	}
	for round := 0; round < refinementRounds; round++ {
		next := make([][]byte, len(nodes))
		for i := range nodes {
			hasher := sha256.New()
			hasher.Write(colours[i])
			for _, e := range nodes[i].edges {
				if e.target < 0 || !counter[e.target] {
					continue
				}
				fmt.Fprintf(hasher, "\x00%d|%d|%d|", e.offset, e.kind, e.addend)
				hasher.Write(colours[e.target])
			}
			next[i] = hasher.Sum(nil)[:16]
		}
		colours = next
	}
	for i := range nodes {
		nodes[i].record.Colour = hex.EncodeToString(colours[i][:12])
	}
}

// dumpSymbols prints the masked bytes and normalized relocations of the named
// symbols, so two programs' copies of one symbol can be diffed directly.
func dumpSymbols(program string, o *obj.Object, want map[string]bool) {
	masked := map[obj.SecKind][]byte{}
	for _, kind := range contentSections {
		copyOf := append([]byte(nil), sectionBytes(o, kind)...)
		for _, relocation := range sectionRelocs(o, kind) {
			maskReloc(copyOf, relocation)
		}
		masked[kind] = copyOf
	}
	for _, symbol := range o.Syms {
		if !want[symbol.Name] || symbol.Size == 0 {
			continue
		}
		body := masked[symbol.Section]
		if body == nil {
			continue
		}
		start, end := int(symbol.Value), int(symbol.Value+symbol.Size)
		if end > len(body) {
			end = len(body)
		}
		fmt.Printf("### %s %s %s size=%d\n", program, symbol.Name, sectionName(symbol.Section), symbol.Size)
		for offset := start; offset < end; offset += 16 {
			limit := offset + 16
			if limit > end {
				limit = end
			}
			fmt.Printf("%s %06x %x\n", symbol.Name, offset-start, body[offset:limit])
		}
		list := append([]obj.Reloc(nil), sectionRelocs(o, symbol.Section)...)
		sort.Slice(list, func(i, j int) bool { return list[i].Offset < list[j].Offset })
		for _, relocation := range list {
			if relocation.Offset < uint64(start) || relocation.Offset >= uint64(end) {
				continue
			}
			fmt.Printf("%s reloc %06x %-20s %s+%d\n", symbol.Name, relocation.Offset-uint64(start), relocName(relocation.Type), relocation.Sym, relocation.Addend)
		}
	}
}

// censusModule reports the IR the back end has to chew through and the data
// items whose value is a whole-module layout delta rather than a relocation.
func censusModule(program string, module *ir.Module) {
	blocks, instructions := 0, 0
	for _, function := range module.Funcs {
		if function == nil {
			continue
		}
		for _, block := range function.Blocks {
			blocks++
			instructions += len(block.Instrs)
		}
	}
	relative := 0
	relativeBases := map[string]int{}
	owners := 0
	for _, data := range module.Data {
		owns := false
		for _, item := range data.Items {
			if item.Sym != "" && item.RelativeTo != "" {
				relative++
				relativeBases[item.RelativeTo]++
				owns = true
			}
		}
		if owns {
			owners++
		}
	}
	fmt.Printf("%s: funcs=%d blocks=%d instrs=%d data=%d relative-items=%d in %d data symbols; bases=%v\n",
		program, len(module.Funcs), blocks, instructions, len(module.Data), relative, owners, relativeBases)
}

func main() {
	outDir := flag.String("out", "", "directory to write per-program JSON records into")
	dump := flag.String("dump", "", "comma-separated symbol names to dump instead of summarizing")
	fixups := flag.Bool("fixups", false, "census the module's relative data references and IR sizes instead of compiling an object")
	timing := flag.Bool("timing", false, "report front-end and back-end wall clock")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: sepcompile [-out dir] file.go...")
		os.Exit(2)
	}
	for _, input := range flag.Args() {
		src, err := os.ReadFile(input)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sepcompile: %v\n", err)
			os.Exit(1)
		}
		frontEndStart := time.Now()
		module, err := goc.CompileExecutableFor(goc.TargetARM64, filepath.Base(input), src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sepcompile: %s: compile: %v\n", input, err)
			continue
		}
		frontEnd := time.Since(frontEndStart)
		if *fixups {
			censusModule(filepath.Base(input), module)
			continue
		}
		backEndStart := time.Now()
		object, err := arm64.CompileToObject(module)
		backEnd := time.Since(backEndStart)
		if *timing {
			fmt.Printf("%s: frontend=%.3fs backend=%.3fs total=%.3fs\n",
				filepath.Base(input), frontEnd.Seconds(), backEnd.Seconds(), (frontEnd + backEnd).Seconds())
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "sepcompile: %s: object: %v\n", input, err)
			continue
		}
		if *dump != "" {
			want := map[string]bool{}
			for _, name := range strings.Split(*dump, ",") {
				want[name] = true
			}
			dumpSymbols(filepath.Base(input), object, want)
			continue
		}
		record := analyze(filepath.Base(input), object)
		fmt.Printf("%s: text=%d data=%d rodata=%d relro=%d bss=%d syms=%d undef=%d\n",
			record.Program, record.TextSize, record.DataSize, record.RodataN,
			record.RelroN, record.BssSize, len(record.Symbols), len(record.Undefined))
		if *outDir == "" {
			continue
		}
		if err := os.MkdirAll(*outDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "sepcompile: %v\n", err)
			os.Exit(1)
		}
		data, err := json.Marshal(record)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sepcompile: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(filepath.Join(*outDir, record.Program+".json"), data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "sepcompile: %v\n", err)
			os.Exit(1)
		}
	}
}
