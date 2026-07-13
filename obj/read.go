package obj

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
)

// ReadELF parses an ELF64 relocatable object (the inverse of MarshalELF) into an
// Object: its .text/.data, defined and undefined symbols, and the relocations
// against .text and .data. It is how the linker ingests architecture .o files —
// whether produced by cg12 or another toolchain — into the common object model.
func ReadELF(data []byte) (*Object, error) {
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if f.Type != elf.ET_REL {
		return nil, fmt.Errorf("obj: not a relocatable object (type %v)", f.Type)
	}
	o := &Object{Machine: uint16(f.Machine)}

	if s := f.Section(".text"); s != nil {
		if o.Text, err = s.Data(); err != nil {
			return nil, err
		}
	}
	if s := f.Section(".data"); s != nil {
		if o.Data, err = s.Data(); err != nil {
			return nil, err
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
		if sym, ok := elfSymbol(f, s); ok {
			o.Syms = append(o.Syms, sym)
		}
	}

	if o.Relocs, err = readRela(f, ".rela.text", names); err != nil {
		return nil, err
	}
	if o.DataRelocs, err = readRela(f, ".rela.data", names); err != nil {
		return nil, err
	}
	return o, nil
}

// elfSymbol converts one ELF symbol to an obj.Sym, reporting false for entries
// the object model does not track (section/file symbols, unnamed locals).
func elfSymbol(f *elf.File, s elf.Symbol) (Sym, bool) {
	typ := elf.ST_TYPE(s.Info)
	if typ == elf.STT_SECTION || typ == elf.STT_FILE || s.Name == "" {
		return Sym{}, false
	}
	sym := Sym{
		Name:   s.Name,
		Value:  s.Value,
		Size:   s.Size,
		Global: elf.ST_BIND(s.Info) != elf.STB_LOCAL,
		Func:   typ == elf.STT_FUNC,
		TLS:    typ == elf.STT_TLS,
	}
	switch {
	case s.Section == elf.SHN_UNDEF:
		sym.Section = SecUndef
	case int(s.Section) < len(f.Sections):
		switch f.Sections[s.Section].Name {
		case ".text":
			sym.Section = SecText
		case ".data", ".bss":
			sym.Section = SecData
		case ".rodata":
			sym.Section = SecRodata
		case ".cg12_stackmaps":
			sym.Section = SecStackMap
		default:
			sym.Section = SecData
		}
	default:
		sym.Section = SecUndef
	}
	return sym, true
}

// readRela decodes a SHT_RELA section (Elf64_Rela entries) into obj.Relocs,
// resolving each entry's symbol index to a name.
func readRela(f *elf.File, name string, names []string) ([]Reloc, error) {
	s := f.Section(name)
	if s == nil {
		return nil, nil
	}
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
			Offset: binary.LittleEndian.Uint64(b[off:]),
			Sym:    sym,
			Type:   uint32(info),
			Addend: int64(binary.LittleEndian.Uint64(b[off+16:])),
		})
	}
	return out, nil
}
