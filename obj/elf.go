// Package obj writes ELF64 relocatable object files (ET_REL) directly, so a
// backend can produce a linkable .o without an external assembler. The container
// is architecture-independent — the caller supplies the machine type and
// architecture-specific relocation type numbers (AArch64 values are provided as
// constants for convenience).
package obj

import "fmt"

// ELF machine types.
const (
	EM_AARCH64 = 183
	EM_X86_64  = 62
)

// A few AArch64 relocation types the backend needs.
const (
	R_AARCH64_ABS64            = 257 // 64-bit absolute address
	R_AARCH64_CALL26           = 283 // bl to a 26-bit PC-relative target
	R_AARCH64_JUMP26           = 282 // b to a 26-bit PC-relative target
	R_AARCH64_ADR_PREL_PG_HI21 = 275 // adrp: page of a symbol
	R_AARCH64_ADD_ABS_LO12_NC  = 277 // add: low 12 bits of a symbol

	// Thread-local storage, local-exec model: add the variable's offset from the
	// thread pointer (read via MRS TPIDR_EL0).
	R_AARCH64_TLSLE_ADD_TPREL_HI12    = 549
	R_AARCH64_TLSLE_ADD_TPREL_LO12_NC = 550
)

// isTLSReloc reports whether typ references a thread-local symbol (so the symbol
// must be typed STT_TLS).
func isTLSReloc(typ uint32) bool {
	return typ == R_AARCH64_TLSLE_ADD_TPREL_HI12 || typ == R_AARCH64_TLSLE_ADD_TPREL_LO12_NC
}

// SecKind names the section a symbol is defined in (or that it is undefined).
type SecKind uint8

const (
	SecUndef    SecKind = iota // an external symbol resolved at link time
	SecText                    // defined in .text
	SecData                    // defined in .data
	SecRodata                  // defined in .rodata
	SecStackMap                // defined in .cg12_stackmaps
)

// Sym is a symbol-table entry.
type Sym struct {
	Name    string
	Section SecKind
	Value   uint64 // offset within its section
	Size    uint64
	Global  bool // global (visible to the linker) vs local
	Func    bool // STT_FUNC vs STT_OBJECT
	TLS     bool // STT_TLS: a thread-local symbol
}

// Reloc is a relocation applied to a section (.text or .data).
type Reloc struct {
	Offset uint64 // where in the section to patch
	Sym    string // target symbol name
	Type   uint32 // architecture relocation type (e.g. R_AARCH64_CALL26)
	Addend int64
}

// Object is the content of a relocatable object: code, data, symbols, and the
// relocations that reference them.
type Object struct {
	Machine    uint16
	Text       []byte
	Data       []byte
	Rodata     []byte
	Syms       []Sym
	Relocs     []Reloc // relocations against .text
	DataRelocs []Reloc // relocations against .data (e.g. a pointer to a symbol)

	// Optional DWARF debug sections. When DebugLine is non-empty the writer emits
	// .debug_abbrev/.debug_info/.debug_line/.debug_loc (and their .rela sections).
	// Build them with SetDWARF.
	DebugAbbrev     []byte
	DebugInfo       []byte
	DebugLine       []byte
	DebugLoc        []byte
	DebugInfoRelocs []Reloc // relocations against .debug_info
	DebugLineRelocs []Reloc // relocations against .debug_line

	// Optional GC stack-map section (.cg12_stackmaps): safepoint PCs and the
	// live-root locations at each, for a garbage collector to scan roots.
	StackMap       []byte
	StackMapRelocs []Reloc
}

// ELF constants.
const (
	shtNull     = 0
	shtProgbits = 1
	shtSymtab   = 2
	shtStrtab   = 3
	shtRela     = 4

	shfWrite     = 0x1
	shfAlloc     = 0x2
	shfExecinstr = 0x4

	stbLocal  = 0
	stbGlobal = 1
	sttNotype = 0
	sttObject = 1
	sttFunc   = 2
	sttTLS    = 6

	shnUndef = 0

	etRel       = 1
	evCurrent   = 1
	elfclass64  = 2
	elfdata2lsb = 1
)

// MarshalELF serializes the object to ELF64 relocatable-object bytes.
func (o *Object) MarshalELF() ([]byte, error) {
	// String table for section names.
	shstr := &strtab{}
	// String table for symbol names.
	str := &strtab{}

	// Build the ordered symbol list: the null symbol, then locals, then globals
	// (ELF requires locals to precede globals). Undefined symbols referenced by
	// relocations but not defined here are added as external globals.
	defined := make(map[string]bool, len(o.Syms))
	for _, s := range o.Syms {
		defined[s.Name] = true
	}
	var locals, globals []Sym
	for _, s := range o.Syms {
		if s.Global {
			globals = append(globals, s)
		} else {
			locals = append(locals, s)
		}
	}
	// An undefined symbol referenced by a TLS relocation must be typed STT_TLS.
	tlsSym := map[string]bool{}
	allRelocs := append(append(append([]Reloc{}, o.Relocs...), o.DataRelocs...), o.DebugInfoRelocs...)
	for _, rl := range allRelocs {
		if isTLSReloc(rl.Type) {
			tlsSym[rl.Sym] = true
		}
	}
	seen := make(map[string]bool)
	for _, rl := range allRelocs {
		if !defined[rl.Sym] && !seen[rl.Sym] {
			seen[rl.Sym] = true
			globals = append(globals, Sym{Name: rl.Sym, Section: SecUndef, Global: true, TLS: tlsSym[rl.Sym]})
		}
	}

	// Section indices, assigned in construction order. The DWARF sections are
	// present only when debug info was supplied.
	hasDwarf := len(o.DebugLine) > 0
	idx := 0
	next := func() int { i := idx; idx++; return i }
	secNull := next()
	secText := next()
	secRela := next()
	secData := next()
	secRelaData := next()
	var secDebugAbbrev, secDebugInfo, secRelaInfo, secDebugLine, secRelaLine, secDebugLoc int
	if hasDwarf {
		secDebugAbbrev = next()
		secDebugInfo = next()
		secRelaInfo = next()
		secDebugLine = next()
		secRelaLine = next()
		secDebugLoc = next()
	}
	hasStackMap := len(o.StackMap) > 0
	var secStackMap, secRelaStackMap int
	if hasStackMap {
		secStackMap = next()
		secRelaStackMap = next()
	}
	secSymtab := next()
	secStrtab := next()
	secShstrtab := next()
	numSec := idx

	symIndex := map[string]int{}
	symtab := &elfBuf{}
	symtab.pad(24) // index 0: the null symbol
	i := 1
	writeSym := func(s Sym) {
		symIndex[s.Name] = i
		i++
		shndx := uint16(shnUndef)
		switch s.Section {
		case SecText:
			shndx = uint16(secText)
		case SecData:
			shndx = uint16(secData)
		case SecStackMap:
			shndx = uint16(secStackMap)
		}
		typ := byte(sttNotype)
		switch {
		case s.TLS:
			typ = sttTLS
		case s.Func:
			typ = sttFunc
		case s.Section == SecData:
			typ = sttObject
		}
		bind := byte(stbLocal)
		if s.Global {
			bind = stbGlobal
		}
		symtab.u32(str.add(s.Name))
		symtab.u8((bind << 4) | typ)
		symtab.u8(0) // st_other
		symtab.u16(shndx)
		symtab.u64(s.Value)
		symtab.u64(s.Size)
	}
	for _, s := range locals {
		writeSym(s)
	}
	firstGlobal := i
	for _, s := range globals {
		writeSym(s)
	}

	// Relocations against .text and .data.
	encodeRela := func(relocs []Reloc) (*elfBuf, error) {
		rela := &elfBuf{}
		for _, rl := range relocs {
			idx, ok := symIndex[rl.Sym]
			if !ok {
				return nil, fmt.Errorf("obj: relocation references unknown symbol %q", rl.Sym)
			}
			rela.u64(rl.Offset)
			rela.u64(uint64(idx)<<32 | uint64(rl.Type))
			rela.i64(rl.Addend)
		}
		return rela, nil
	}
	rela, err := encodeRela(o.Relocs)
	if err != nil {
		return nil, err
	}
	relaData, err := encodeRela(o.DataRelocs)
	if err != nil {
		return nil, err
	}
	relaInfo, err := encodeRela(o.DebugInfoRelocs)
	if err != nil {
		return nil, err
	}
	relaLine, err := encodeRela(o.DebugLineRelocs)
	if err != nil {
		return nil, err
	}
	relaStackMap, err := encodeRela(o.StackMapRelocs)
	if err != nil {
		return nil, err
	}

	// Assemble sections.
	secs := make([]section, numSec)
	secs[secNull] = section{}
	secs[secText] = section{name: ".text", typ: shtProgbits, flags: shfAlloc | shfExecinstr, addralign: 4, data: o.Text}
	secs[secRela] = section{name: ".rela.text", typ: shtRela, link: uint32(secSymtab), info: uint32(secText), addralign: 8, entsize: 24, data: rela.b}
	secs[secData] = section{name: ".data", typ: shtProgbits, flags: shfAlloc | shfWrite, addralign: 8, data: o.Data}
	secs[secRelaData] = section{name: ".rela.data", typ: shtRela, link: uint32(secSymtab), info: uint32(secData), addralign: 8, entsize: 24, data: relaData.b}
	if hasDwarf {
		secs[secDebugAbbrev] = section{name: ".debug_abbrev", typ: shtProgbits, addralign: 1, data: o.DebugAbbrev}
		secs[secDebugInfo] = section{name: ".debug_info", typ: shtProgbits, addralign: 1, data: o.DebugInfo}
		secs[secRelaInfo] = section{name: ".rela.debug_info", typ: shtRela, link: uint32(secSymtab), info: uint32(secDebugInfo), addralign: 8, entsize: 24, data: relaInfo.b}
		secs[secDebugLine] = section{name: ".debug_line", typ: shtProgbits, addralign: 1, data: o.DebugLine}
		secs[secRelaLine] = section{name: ".rela.debug_line", typ: shtRela, link: uint32(secSymtab), info: uint32(secDebugLine), addralign: 8, entsize: 24, data: relaLine.b}
		secs[secDebugLoc] = section{name: ".debug_loc", typ: shtProgbits, addralign: 1, data: o.DebugLoc}
	}
	if hasStackMap {
		secs[secStackMap] = section{name: ".cg12_stackmaps", typ: shtProgbits, flags: shfAlloc, addralign: 8, data: o.StackMap}
		secs[secRelaStackMap] = section{name: ".rela.cg12_stackmaps", typ: shtRela, link: uint32(secSymtab), info: uint32(secStackMap), addralign: 8, entsize: 24, data: relaStackMap.b}
	}
	secs[secSymtab] = section{name: ".symtab", typ: shtSymtab, link: uint32(secStrtab), info: uint32(firstGlobal), addralign: 8, entsize: 24, data: symtab.b}
	secs[secStrtab] = section{name: ".strtab", typ: shtStrtab, addralign: 1, data: str.b}
	secs[secShstrtab] = section{name: ".shstrtab", typ: shtStrtab, addralign: 1}

	for i := range secs {
		secs[i].nameOff = shstr.add(secs[i].name)
	}
	secs[secShstrtab].data = shstr.b // finalized after all names are interned

	// Lay out the file: header, section data, then the section header table.
	out := &elfBuf{}
	out.pad(64) // ELF header, filled in at the end
	for i := range secs {
		if secs[i].typ == shtNull || len(secs[i].data) == 0 && secs[i].typ != shtProgbits {
			continue
		}
		out.align(secs[i].addralign)
		secs[i].offset = uint64(len(out.b))
		out.bytes(secs[i].data)
	}
	out.align(8)
	shoff := uint64(len(out.b))
	for i := range secs {
		s := secs[i]
		out.u32(s.nameOff)
		out.u32(s.typ)
		out.u64(s.flags)
		out.u64(0) // sh_addr
		out.u64(s.offset)
		out.u64(uint64(len(s.data)))
		out.u32(s.link)
		out.u32(s.info)
		out.u64(s.addralign)
		out.u64(s.entsize)
	}

	writeHeader(out.b, o.Machine, shoff, numSec, secShstrtab)
	return out.b, nil
}

type section struct {
	name      string
	typ       uint32
	flags     uint64
	link      uint32
	info      uint32
	addralign uint64
	entsize   uint64
	data      []byte
	nameOff   uint32
	offset    uint64
}

// writeHeader fills the 64-byte ELF header in place.
func writeHeader(b []byte, machine uint16, shoff uint64, shnum, shstrndx int) {
	h := &elfBuf{b: b[:0:64]}
	h.bytes([]byte{0x7f, 'E', 'L', 'F', elfclass64, elfdata2lsb, evCurrent, 0})
	h.pad(8) // rest of e_ident
	h.u16(etRel)
	h.u16(machine)
	h.u32(evCurrent)
	h.u64(0) // e_entry
	h.u64(0) // e_phoff
	h.u64(shoff)
	h.u32(0)  // e_flags
	h.u16(64) // e_ehsize
	h.u16(0)  // e_phentsize
	h.u16(0)  // e_phnum
	h.u16(64) // e_shentsize
	h.u16(uint16(shnum))
	h.u16(uint16(shstrndx))
}

// --- byte buffer -----------------------------------------------------------

type elfBuf struct{ b []byte }

func (w *elfBuf) u8(v byte)      { w.b = append(w.b, v) }
func (w *elfBuf) u16(v uint16)   { w.b = append(w.b, byte(v), byte(v>>8)) }
func (w *elfBuf) u32(v uint32)   { w.b = append(w.b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24)) }
func (w *elfBuf) i64(v int64)    { w.u64(uint64(v)) }
func (w *elfBuf) bytes(p []byte) { w.b = append(w.b, p...) }
func (w *elfBuf) pad(n int)      { w.b = append(w.b, make([]byte, n)...) }

func (w *elfBuf) u64(v uint64) {
	w.b = append(w.b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
}

func (w *elfBuf) align(a uint64) {
	if a < 1 {
		a = 1
	}
	for uint64(len(w.b))%a != 0 {
		w.b = append(w.b, 0)
	}
}

// --- string table ----------------------------------------------------------

type strtab struct {
	b   []byte
	off map[string]uint32
}

// add interns a name and returns its offset. Offset 0 is always the empty
// string, per the ELF string-table convention.
func (s *strtab) add(name string) uint32 {
	if s.off == nil {
		s.off = map[string]uint32{}
		s.b = []byte{0}
	}
	if o, ok := s.off[name]; ok {
		return o
	}
	o := uint32(len(s.b))
	s.off[name] = o
	s.b = append(s.b, name...)
	s.b = append(s.b, 0)
	return o
}
