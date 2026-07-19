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
	R_AARCH64_ADR_PREL_LO21    = 274 // adr: 21-bit PC-relative address of a symbol
	R_AARCH64_ADR_PREL_PG_HI21 = 275 // adrp: page of a symbol
	R_AARCH64_ADD_ABS_LO12_NC  = 277 // add: low 12 bits of a symbol

	// Thread-local storage, local-exec model: add the variable's offset from the
	// thread pointer (read via MRS TPIDR_EL0).
	// The pair is HI12 then LO12_NC: the offset is split across two adds. The
	// checking LO12 (550) is for an offset that fits 12 bits all on its own, so
	// pairing it with a HI12 makes a linker reject any block over 4 KiB.
	R_AARCH64_TLSLE_ADD_TPREL_HI12    = 549
	R_AARCH64_TLSLE_ADD_TPREL_LO12_NC = 551

	// Thread-local storage, initial-exec model: the offset from the thread pointer
	// is not known until load time, so it is read from a GOT slot the loader fills.
	R_AARCH64_TLSIE_ADR_GOTTPREL_PAGE21   = 541  // adrp: page of the slot
	R_AARCH64_TLSIE_LD64_GOTTPREL_LO12_NC = 542  // ldr: low 12 bits of the slot
	R_AARCH64_TLS_TPREL64                 = 1030 // the slot itself: the loader stores the offset

	// Thread-local storage, general-dynamic model: for a variable whose storage may
	// not exist yet (one in a library brought in by dlopen). The code passes the
	// address of a two-word descriptor to __tls_get_addr, which finds or allocates
	// this thread's block for that module and returns the variable's address.
	R_AARCH64_TLSGD_ADR_PAGE21  = 513  // adrp: page of the descriptor
	R_AARCH64_TLSGD_ADD_LO12_NC = 514  // add: low 12 bits of the descriptor
	R_AARCH64_TLS_DTPMOD64      = 1028 // descriptor word 0: which module owns it
	R_AARCH64_TLS_DTPREL64      = 1029 // descriptor word 1: its offset within that module
)

// A few x86-64 relocation types the backend needs.
const (
	R_X86_64_64    = 1  // 64-bit absolute address (data pointers, stack-map PCs)
	R_X86_64_PC32  = 2  // 32-bit PC-relative (RIP-relative data/code references)
	R_X86_64_PLT32 = 4  // 32-bit PC-relative to the PLT (call/jmp targets)
	R_X86_64_32    = 10 // 32-bit zero-extended absolute
	R_X86_64_32S   = 11 // 32-bit sign-extended absolute

	// Thread-local storage, local-exec model: a 32-bit offset from the thread
	// pointer (read via %fs).
	R_X86_64_TPOFF32 = 23

	// Thread-local storage, initial-exec model: the offset is read from a GOT slot
	// the loader fills.
	R_X86_64_GOTTPOFF = 22 // RIP-relative reference to the slot
	R_X86_64_TPOFF64  = 18 // the slot itself: the loader stores the offset
)

// isX86TLSReloc reports whether typ references a thread-local x86-64 symbol.
func isX86TLSReloc(typ uint32) bool { return typ == R_X86_64_TPOFF32 }

// isTLSGotReloc reports whether typ reads a thread-local's offset out of a GOT
// slot (the initial-exec model) rather than encoding it directly.
func isTLSGotReloc(typ uint32) bool {
	switch typ {
	case R_AARCH64_TLSIE_ADR_GOTTPREL_PAGE21, R_AARCH64_TLSIE_LD64_GOTTPREL_LO12_NC, R_X86_64_GOTTPOFF:
		return true
	}
	return false
}

// isTLSGdReloc reports whether typ addresses a general-dynamic descriptor.
func isTLSGdReloc(typ uint32) bool {
	return typ == R_AARCH64_TLSGD_ADR_PAGE21 || typ == R_AARCH64_TLSGD_ADD_LO12_NC
}

// tlsGotSlotType is the machine's relocation for a thread-local GOT slot: the
// loader works out the variable's offset from the thread pointer and stores it.
func tlsGotSlotType(machine uint16) uint32 {
	switch machine {
	case EM_AARCH64:
		return R_AARCH64_TLS_TPREL64
	case EM_X86_64:
		return R_X86_64_TPOFF64
	}
	return 0
}

// isTLSReloc reports whether typ references a thread-local symbol (so the symbol
// must be typed STT_TLS).
func isTLSReloc(typ uint32) bool {
	return typ == R_AARCH64_TLSLE_ADD_TPREL_HI12 || typ == R_AARCH64_TLSLE_ADD_TPREL_LO12_NC
}

// isAnyTLSReloc reports whether typ references a thread-local symbol in any model,
// which is what makes the symbol STT_TLS.
func isAnyTLSReloc(typ uint32) bool {
	return isTLSReloc(typ) || isX86TLSReloc(typ) || isTLSGotReloc(typ) || isTLSGdReloc(typ)
}

// SecKind names the section a symbol is defined in (or that it is undefined).
type SecKind uint8

const (
	SecUndef    SecKind = iota // an external symbol resolved at link time
	SecText                    // defined in .text
	SecData                    // defined in .data
	SecRodata                  // defined in .rodata
	SecRelro                   // defined in .data.rel.ro (read-only, but holds an address)
	SecStackMap                // defined in .cg12_stackmaps
	SecTdata                   // defined in .tdata (thread-local)
	SecBss                     // defined in .bss (zero-filled, occupies no file space)
	SecTbss                    // defined in .tbss (thread-local, zero-filled)
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
	Machine uint16
	Text    []byte
	Data    []byte

	// Rodata is data the program may read and not write: a const object, a string
	// literal. It is a section of its own because that is the only way to say so
	// -- the promise is kept by the page it lands on, not by the type it had in
	// the source, and C leaves writing through a cast to it undefined precisely so
	// an implementation may put it where the hardware refuses.
	Rodata []byte

	// Relro is read-only data that holds an address: `static const char *const
	// p[] = {...}`. It is const to the program and yet cannot be finished at link
	// time, because in a position-independent image the address it holds is not
	// known until the loader picks a base -- and .rodata is mapped without write
	// permission, so there is nowhere to write it.
	//
	// So it gets a section that is writable while the loader relocates it and
	// read-only afterwards, which PT_GNU_RELRO arranges (see MarshalDynamic). The
	// program never sees it writable. In a fixed-base image no such datum arises:
	// every address is a link-time constant, so it stays in .rodata.
	Relro []byte

	// Tdata is the thread-local initialization image (.tdata): not data used in
	// place, but the bytes each new thread's TLS block starts life as.
	Tdata []byte

	// BssSize and TbssSize are zero-filled regions that take up no room in the
	// file: the loader supplies the zeroes. .bss follows .data in memory, .tbss
	// follows .tdata in each thread's block.
	BssSize  int
	TbssSize int

	// The alignment each section's contents require. A datum is padded to its own
	// alignment within its section, which only means anything if the section
	// itself is placed at least that well -- align a 16-byte value inside a
	// section the image put on an 8-byte boundary and it is 16-aligned relative to
	// nothing. Zero means no requirement.
	//
	// TlsAlign covers both .tdata and .tbss, which are one block per thread.
	DataAlign   int
	RodataAlign int
	RelroAlign  int
	BssAlign    int
	TlsAlign    int

	Syms       []Sym
	Relocs     []Reloc // relocations against .text
	DataRelocs []Reloc // relocations against .data (e.g. a pointer to a symbol)
	// RodataRelocs are relocations against .rodata. They are a separate list
	// because a relocation's offset is relative to its own section, so a .rodata
	// offset filed under .rela.data patches whatever happens to be at that offset
	// in .data instead.
	RodataRelocs []Reloc
	// RelroRelocs are relocations against .data.rel.ro, separate for the same
	// reason. Unlike .rodata's, these are expected: a datum is in this section
	// precisely because it has one.
	RelroRelocs []Reloc

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
	shtNobits   = 8
	shtRela     = 4

	shfWrite     = 0x1
	shfAlloc     = 0x2
	shfExecinstr = 0x4
	shfTLS       = 0x400

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
	// Every relocation section that encodeRela emits below must be scanned here, or
	// a symbol referenced only from a section left out (a function pointer in
	// read-only data, say) is never added as undefined and encoding it fails.
	var allRelocs []Reloc
	allRelocs = append(allRelocs, o.Relocs...)
	allRelocs = append(allRelocs, o.DataRelocs...)
	allRelocs = append(allRelocs, o.RodataRelocs...)
	allRelocs = append(allRelocs, o.RelroRelocs...)
	allRelocs = append(allRelocs, o.DebugInfoRelocs...)
	allRelocs = append(allRelocs, o.DebugLineRelocs...)
	for _, rl := range allRelocs {
		if isAnyTLSReloc(rl.Type) {
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
	secRodata, secRelaRodata := -1, -1
	if len(o.Rodata) > 0 {
		secRodata = next()
		secRelaRodata = next()
	}
	secRelro, secRelaRelro := -1, -1
	if len(o.Relro) > 0 {
		secRelro = next()
		secRelaRelro = next()
	}
	secBss := -1
	if o.BssSize > 0 {
		secBss = next()
	}
	secTdata, secTbss := -1, -1
	if len(o.Tdata) > 0 {
		secTdata = next()
	}
	if o.TbssSize > 0 {
		secTbss = next()
	}
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
		case SecRodata:
			shndx = uint16(secRodata)
		case SecRelro:
			shndx = uint16(secRelro)
		case SecBss:
			shndx = uint16(secBss)
		case SecTdata:
			shndx = uint16(secTdata)
		case SecTbss:
			shndx = uint16(secTbss)
		case SecStackMap:
			shndx = uint16(secStackMap)
		}
		// A symbol defined in any data section is an object; only .rodata was
		// missing here, so a const array or a string literal went out STT_NOTYPE --
		// which is what an assembly label with no .type directive looks like, not
		// what data looks like. gcc emits STT_OBJECT for the same source.
		//
		// No linker has been found that minds: GNU ld and lld both resolve against
		// the NOTYPE symbol, and a shared library exporting one produces the same
		// relocations as gcc's. This is fidelity rather than a fix -- but the type
		// is a claim about what the symbol is, and the claim was false.
		typ := byte(sttNotype)
		switch {
		case s.TLS:
			typ = sttTLS
		case s.Func:
			typ = sttFunc
		case s.Section == SecData || s.Section == SecBss ||
			s.Section == SecRelro || s.Section == SecRodata:
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
	relaRodata, err := encodeRela(o.RodataRelocs)
	if err != nil {
		return nil, err
	}
	relaRelro, err := encodeRela(o.RelroRelocs)
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
	secs[secData] = section{name: ".data", typ: shtProgbits, flags: shfAlloc | shfWrite, addralign: uint64(alignOr(o.DataAlign, 8)), data: o.Data}
	if secRodata >= 0 {
		// No shfWrite: that flag is the whole point of the section.
		secs[secRodata] = section{name: ".rodata", typ: shtProgbits, flags: shfAlloc, addralign: uint64(alignOr(o.RodataAlign, 8)), data: o.Rodata}
		secs[secRelaRodata] = section{name: ".rela.rodata", typ: shtRela, link: uint32(secSymtab), info: uint32(secRodata), addralign: 8, entsize: 24, data: relaRodata.b}
	}
	if secRelro >= 0 {
		// shfWrite, and yet the section's name is a promise it is not: it is
		// writable so the loader can relocate it, and PT_GNU_RELRO takes the write
		// permission away before the program runs. A static linker reading this
		// object recognises the name and keeps it apart from ordinary .data.
		secs[secRelro] = section{name: ".data.rel.ro", typ: shtProgbits, flags: shfAlloc | shfWrite, addralign: uint64(alignOr(o.RelroAlign, 8)), data: o.Relro}
		secs[secRelaRelro] = section{name: ".rela.data.rel.ro", typ: shtRela, link: uint32(secSymtab), info: uint32(secRelro), addralign: 8, entsize: 24, data: relaRelro.b}
	}
	secs[secRelaData] = section{name: ".rela.data", typ: shtRela, link: uint32(secSymtab), info: uint32(secData), addralign: 8, entsize: 24, data: relaData.b}
	if secBss >= 0 {
		secs[secBss] = section{name: ".bss", typ: shtNobits, flags: shfAlloc | shfWrite, addralign: uint64(alignOr(o.BssAlign, 8)), nobits: uint64(o.BssSize)}
	}
	if secTdata >= 0 {
		secs[secTdata] = section{name: ".tdata", typ: shtProgbits, flags: shfAlloc | shfWrite | shfTLS, addralign: uint64(tlsAlignOf(o)), data: o.Tdata}
	}
	if secTbss >= 0 {
		secs[secTbss] = section{name: ".tbss", typ: shtNobits, flags: shfAlloc | shfWrite | shfTLS, addralign: uint64(tlsAlignOf(o)), nobits: uint64(o.TbssSize)}
	}
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
		out.bytes(secs[i].data) // a NOBITS section has none, which is the point
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
		if s.typ == shtNobits {
			out.u64(s.nobits)
		} else {
			out.u64(uint64(len(s.data)))
		}
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
	// nobits is the size of a section that occupies run-time space but no file
	// bytes (.bss and .tbss), which therefore has no data to carry.
	nobits  uint64
	nameOff uint32
	offset  uint64
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

// AlignInt rounds n up to the next multiple of a (a <= 0 means no alignment).
func AlignInt(n, a int) int {
	if a < 1 {
		return n
	}
	return (n + a - 1) &^ (a - 1)
}

// tlsAlignOf is an object's TLS block alignment, defaulting to a single byte.
func tlsAlignOf(o *Object) int { return alignOr(o.TlsAlign, 1) }

// bssStart is where .bss begins relative to the start of .data: after .data's
// bytes, rounded up to what .bss's own contents require. .bss has no bytes in
// the file, so this is a memory offset only.
func bssStart(o *Object) int { return AlignInt(len(o.Data), alignOr(o.BssAlign, 1)) }

// alignOr is a recorded alignment, or def where nothing asked for one. A section
// with no requirement still needs a positive addralign in its header.
func alignOr(a, def int) int {
	if a < def {
		return def
	}
	return a
}
