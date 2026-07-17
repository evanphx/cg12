package obj

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"strings"
)

// ReadELF parses an ELF64 relocatable object (the inverse of MarshalELF) into an
// Object: its .text/.data/.bss, defined and undefined symbols, and the
// relocations against them. It is how the linker ingests architecture .o files —
// whether produced by cg12 or another toolchain — into the common object model.
//
// The object model has a fixed, small set of sections, and a real toolchain
// emits many more. Anything that cannot be mapped onto one of ours is an error
// naming the section: the alternative is placing its bytes and symbols somewhere
// they do not belong, which links cleanly and reads the wrong memory.
func ReadELF(data []byte) (*Object, error) {
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if f.Type != elf.ET_REL {
		return nil, fmt.Errorf("obj: not a relocatable object (type %v)", f.Type)
	}
	o := &Object{Machine: uint16(f.Machine)}

	// Where each ELF section lands in the object model. A section's symbols and
	// relocations are offset by base, since several ELF sections may merge into
	// one of ours.
	type placed struct {
		kind SecKind
		base uint64
	}
	place := map[elf.SectionIndex]placed{}
	skipped := map[elf.SectionIndex]bool{} // knowingly not carried; their symbols go too

	for i, s := range f.Sections {
		kind, ok := sectionKind(s)
		if !ok {
			if ignorableSection(s) {
				skipped[elf.SectionIndex(i)] = true
				continue
			}
			return nil, fmt.Errorf("obj: this object model has no section for %q", s.Name)
		}
		idx := elf.SectionIndex(i)
		switch kind {
		case SecText:
			place[idx] = placed{kind, uint64(len(o.Text))}
			b, err := s.Data()
			if err != nil {
				return nil, err
			}
			o.Text = append(o.Text, b...)
		case SecData:
			place[idx] = placed{kind, uint64(len(o.Data))}
			b, err := s.Data()
			if err != nil {
				return nil, err
			}
			o.Data = append(o.Data, b...)
			if a := int(s.Addralign); a > o.DataAlign {
				o.DataAlign = a
			}
		case SecRodata:
			place[idx] = placed{kind, uint64(len(o.Rodata))}
			b, err := s.Data()
			if err != nil {
				return nil, err
			}
			o.Rodata = append(o.Rodata, b...)
			if a := int(s.Addralign); a > o.RodataAlign {
				o.RodataAlign = a
			}
		case SecRelro:
			place[idx] = placed{kind, uint64(len(o.Relro))}
			b, err := s.Data()
			if err != nil {
				return nil, err
			}
			o.Relro = append(o.Relro, b...)
			if a := int(s.Addralign); a > o.RelroAlign {
				o.RelroAlign = a
			}
		case SecBss:
			// .bss occupies no file bytes: only a size, and an offset within it.
			place[idx] = placed{kind, uint64(o.BssSize)}
			o.BssSize += int(s.Size)
		case SecTdata:
			place[idx] = placed{kind, uint64(len(o.Tdata))}
			b, err := s.Data()
			if err != nil {
				return nil, err
			}
			o.Tdata = append(o.Tdata, b...)
			if a := int(s.Addralign); a > o.TlsAlign {
				o.TlsAlign = a
			}
		case SecTbss:
			place[idx] = placed{kind, uint64(o.TbssSize)}
			o.TbssSize += int(s.Size)
			if a := int(s.Addralign); a > o.TlsAlign {
				o.TlsAlign = a
			}
		case SecStackMap:
			place[idx] = placed{kind, 0}
		}
	}

	// Symbols, and the index->name table the relocations reference (ELF symbol 0
	// is the reserved null symbol).
	syms, err := f.Symbols()
	if err != nil && err != elf.ErrNoSymbols {
		return nil, err
	}
	names := make([]string, len(syms)+1)
	for i, s := range syms {
		names[i+1] = s.Name
		sym, ok, err := elfSymbol(f, s, func(i elf.SectionIndex) (SecKind, uint64, bool) {
			p, ok := place[i]
			return p.kind, p.base, ok
		}, skipped)
		if err != nil {
			return nil, err
		}
		if ok {
			o.Syms = append(o.Syms, sym)
		}
	}

	// Every mapped section's relocations, not just .rela.text and .rela.data: a
	// real toolchain puts them in .rela.rodata, .rela.data.rel, .rela.text.foo and
	// more, and reading only two names silently drops the rest.
	for i, s := range f.Sections {
		if s.Type != elf.SHT_RELA {
			continue
		}
		target := elf.SectionIndex(s.Info)
		p, ok := place[target]
		if !ok {
			if skipped[target] {
				continue // it relocates something we do not carry
			}
			return nil, fmt.Errorf("obj: %s relocates %q, which this object model does not place",
				s.Name, sectionName(f, target))
		}
		rs, err := readRela(f.Sections[i], names, p.base)
		if err != nil {
			return nil, err
		}
		switch p.kind {
		case SecText:
			o.Relocs = append(o.Relocs, rs...)
		case SecData, SecTdata:
			o.DataRelocs = append(o.DataRelocs, rs...)
		case SecRodata:
			o.RodataRelocs = append(o.RodataRelocs, rs...)
		case SecRelro:
			o.RelroRelocs = append(o.RelroRelocs, rs...)
		default:
			return nil, fmt.Errorf("obj: %s relocates %q, which cannot carry relocations", s.Name, f.Sections[target].Name)
		}
	}
	return o, nil
}

// sectionKind maps an ELF section onto the object model's, or reports false when
// there is none. It matches on the section's role rather than its exact name, so
// that -ffunction-sections' .text.foo and a string pool's .rodata.str1.1 land
// where they belong.
func sectionKind(s *elf.Section) (SecKind, bool) {
	if s.Flags&elf.SHF_ALLOC == 0 {
		return 0, false // not part of the image: debug info, comments, notes
	}
	name := s.Name
	switch {
	case name == ".cg12_stackmaps":
		return SecStackMap, true
	case s.Flags&elf.SHF_TLS != 0:
		if s.Type == elf.SHT_NOBITS {
			return SecTbss, true
		}
		return SecTdata, true
	case s.Flags&elf.SHF_EXECINSTR != 0:
		return SecText, true
	case s.Type == elf.SHT_NOBITS:
		return SecBss, true
	case hasPrefix(name, ".rodata"):
		return SecRodata, true
	case hasPrefix(name, ".data.rel.ro"):
		// Const data holding an address, which gcc emits for `static const char
		// *const []` (as .data.rel.ro.local when nothing outside can name it).
		// Ahead of the .data case below, which its name also starts with -- and
		// distinct from .data.rel, which is writable data that merely needs
		// relocating and stays writable.
		return SecRelro, true
	case hasPrefix(name, ".data", ".init_array", ".fini_array", ".data.rel"):
		return SecData, true
	}
	return 0, false
}

// ignorableSection reports whether a section can be passed over rather than
// refused: either it is not part of the running image at all, or it is something
// we knowingly do not carry.
func ignorableSection(s *elf.Section) bool {
	if s.Type == elf.SHT_NULL || s.Name == "" {
		return true
	}
	if s.Flags&elf.SHF_ALLOC == 0 {
		return true // debug info, comments, notes: not part of the image
	}
	// The unwind tables. Every gcc object has one, so refusing them would refuse
	// every gcc object; dropping them costs unwinding -- backtraces through this
	// code, and C++ exceptions -- and nothing else. cg12 emits none of its own, so
	// this is a limitation we already have rather than one ingesting adds.
	return hasPrefix(s.Name, ".eh_frame", ".eh_frame_hdr", ".gcc_except_table")
}

// hasPrefix reports whether name is one of the given sections or a subsection of
// one (".text.foo" for ".text"), which is how -ffunction-sections and string
// pools name theirs.
func hasPrefix(name string, of ...string) bool {
	for _, p := range of {
		if name == p || strings.HasPrefix(name, p+".") {
			return true
		}
	}
	return false
}

func sectionName(f *elf.File, i elf.SectionIndex) string {
	if int(i) < len(f.Sections) {
		return f.Sections[i].Name
	}
	return fmt.Sprintf("section %d", i)
}

// elfSymbol converts one ELF symbol to an obj.Sym, reporting false for entries
// the object model does not track (section/file symbols, unnamed locals). Its
// value is rebased onto wherever its section was placed.
func elfSymbol(f *elf.File, s elf.Symbol, place func(elf.SectionIndex) (SecKind, uint64, bool), skipped map[elf.SectionIndex]bool) (Sym, bool, error) {
	typ := elf.ST_TYPE(s.Info)
	if typ == elf.STT_SECTION || typ == elf.STT_FILE || s.Name == "" {
		return Sym{}, false, nil
	}
	if isMappingSymbol(s.Name) {
		return Sym{}, false, nil
	}
	bind := elf.ST_BIND(s.Info)
	if bind == elf.STB_WEAK {
		// A weak symbol may be overridden, and may be defined in several objects
		// without that being a duplicate. The model has no way to say either, so
		// calling it strong would turn ordinary C++ or libc objects into spurious
		// duplicate-symbol errors -- or bind a call to the wrong one.
		return Sym{}, false, fmt.Errorf("obj: symbol %q is weak, which this object model cannot represent", s.Name)
	}
	sym := Sym{
		Name:   s.Name,
		Value:  s.Value,
		Size:   s.Size,
		Global: bind != elf.STB_LOCAL,
		Func:   typ == elf.STT_FUNC,
		TLS:    typ == elf.STT_TLS,
	}
	switch {
	case s.Section == elf.SHN_UNDEF:
		sym.Section = SecUndef
		return sym, true, nil
	case s.Section == elf.SHN_ABS || s.Section == elf.SHN_COMMON:
		return Sym{}, false, fmt.Errorf("obj: symbol %q is %v, which this object model cannot represent", s.Name, s.Section)
	}
	if skipped[s.Section] {
		return Sym{}, false, nil // it names something we knowingly do not carry
	}
	kind, base, ok := place(s.Section)
	if !ok {
		return Sym{}, false, fmt.Errorf("obj: symbol %q is defined in %q, which this object model does not place",
			s.Name, sectionName(f, s.Section))
	}
	sym.Section = kind
	sym.Value += base
	return sym, true, nil
}

// isMappingSymbol reports whether a name is an ARM/AArch64 mapping symbol --
// $x, $d, $a, $t marking where code and data alternate within a section. They
// are notes for a disassembler, not symbols anything links against, and there
// are many per object with the same name.
func isMappingSymbol(name string) bool {
	switch name {
	case "$x", "$d", "$a", "$t":
		return true
	}
	// The "$d.1" form, which some assemblers emit.
	return len(name) > 2 && name[0] == '$' && name[2] == '.'
}

// readRela decodes a SHT_RELA section (Elf64_Rela entries) into obj.Relocs,
// resolving each entry's symbol index to a name and offsetting it by where its
// target section was placed.
func readRela(s *elf.Section, names []string, base uint64) ([]Reloc, error) {
	b, err := s.Data()
	if err != nil {
		return nil, err
	}
	var out []Reloc
	for off := 0; off+24 <= len(b); off += 24 {
		info := binary.LittleEndian.Uint64(b[off+8:])
		idx := info >> 32
		var sym string
		if idx < uint64(len(names)) {
			sym = names[idx]
		}
		out = append(out, Reloc{
			Offset: binary.LittleEndian.Uint64(b[off:]) + base,
			Sym:    sym,
			Type:   uint32(info),
			Addend: int64(binary.LittleEndian.Uint64(b[off+16:])),
		})
	}
	return out, nil
}
