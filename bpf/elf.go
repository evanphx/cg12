package bpf

import "encoding/binary"

// This file emits a relocatable eBPF ELF object that libbpf and bpftool load
// directly. The layout mirrors what clang -target bpf produces: one PROGBITS
// section per program (named by its attach section, e.g. "xdp"), a "license"
// section, a ".maps" section of BTF-defined map placeholders, a ".rodata"
// section, a ".BTF" section describing the maps and rodata, a symbol table, and
// R_BPF_64_64 relocations tying each program's map/rodata loads to their symbols.

// ELF and eBPF relocation constants.
const (
	emBPF       = 247
	etRel       = 1
	shtProgbits = 1
	shtSymtab   = 2
	shtStrtab   = 3
	shtRel      = 9
	shfWrite    = 0x1
	shfAlloc    = 0x2
	shfExec     = 0x4
	stbGlobal   = 1
	sttObject   = 1
	sttFunc     = 2
	rBPF6464    = 1
)

// elfSection is one output section, in the order it will appear in the file.
type elfSection struct {
	name           string
	typ, flags     uint32
	data           []byte
	link, info     uint32
	align, entsize uint64
}

type elfWriter struct {
	secs      []elfSection
	secIdx    map[string]uint32
	symtab    []byte         // encoded symbols (starts with the null symbol)
	symName   map[string]int // symbol name -> index
	symStr    []byte         // symbol string table
	symStrO   map[string]uint32
	nsym      int
	localSyms int // count of STB_LOCAL symbols (must precede globals; here only the null symbol)
}

func newELFWriter() *elfWriter {
	w := &elfWriter{
		secIdx:    map[string]uint32{},
		symtab:    make([]byte, 24), // null symbol
		symName:   map[string]int{},
		symStr:    []byte{0},
		symStrO:   map[string]uint32{"": 0},
		nsym:      1,
		localSyms: 1,
	}
	return w
}

func (w *elfWriter) addSection(s elfSection) uint32 {
	idx := uint32(len(w.secs))
	w.secIdx[s.name] = idx
	w.secs = append(w.secs, s)
	return idx
}

func (w *elfWriter) symStrOff(s string) uint32 {
	if o, ok := w.symStrO[s]; ok {
		return o
	}
	o := uint32(len(w.symStr))
	w.symStr = append(w.symStr, s...)
	w.symStr = append(w.symStr, 0)
	w.symStrO[s] = o
	return o
}

// addSym appends a global symbol (name, type, section index, value, size) and
// returns its index.
func (w *elfWriter) addSym(name string, styp uint8, shndx uint32, value, size uint64) int {
	var b [24]byte
	binary.LittleEndian.PutUint32(b[0:], w.symStrOff(name))
	b[4] = byte(stbGlobal<<4) | styp // st_info
	b[5] = 0                         // st_other
	binary.LittleEndian.PutUint16(b[6:], uint16(shndx))
	binary.LittleEndian.PutUint64(b[8:], value)
	binary.LittleEndian.PutUint64(b[16:], size)
	w.symtab = append(w.symtab, b[:]...)
	idx := w.nsym
	w.symName[name] = idx
	w.nsym++
	return idx
}

// ELF renders the object as a relocatable eBPF ELF object file.
func (o *Object) ELF() []byte {
	w := newELFWriter()
	w.addSection(elfSection{name: ""}) // section 0 is null

	// One section per program, plus a symbol naming it.
	type progInfo struct {
		sec    uint32
		code   []byte
		relocs []MapReloc
	}
	var progs []progInfo
	for i, p := range o.Programs {
		name := p.Section
		if name == "" {
			name = ".text"
		}
		code := elfCode(p) // map/rodata loads cleared for libbpf to fill
		sec := w.addSection(elfSection{
			name: name, typ: shtProgbits, flags: shfAlloc | shfExec,
			data: code, align: 8,
		})
		w.addSym(p.Name, sttFunc, sec, 0, uint64(len(code)))
		progs = append(progs, progInfo{sec: sec, code: code, relocs: p.Relocs})
		_ = i
	}

	// license
	if o.License != "" {
		lic := append([]byte(o.License), 0)
		w.addSection(elfSection{name: "license", typ: shtProgbits, flags: shfAlloc, data: lic, align: 1})
	}

	// .maps: a placeholder value per map; each map gets a symbol at its offset.
	var mapSyms []MapDef
	for _, m := range o.Maps {
		if m.Name != rodataName {
			mapSyms = append(mapSyms, m)
		}
	}
	if len(mapSyms) > 0 {
		sec := w.addSection(elfSection{name: ".maps", typ: shtProgbits, flags: shfAlloc, data: make([]byte, len(mapSyms)*32), align: 8})
		for i, m := range mapSyms {
			w.addSym(m.Name, sttObject, sec, uint64(i*32), 32)
		}
	}

	// .rodata: the read-only blob; each global gets a symbol at its offset.
	for _, m := range o.Maps {
		if m.Name != rodataName {
			continue
		}
		sec := w.addSection(elfSection{name: rodataName, typ: shtProgbits, flags: shfAlloc, data: m.Initial, align: 8})
		for _, rv := range o.Rodata {
			w.addSym(rv.Name, sttObject, sec, uint64(rv.Off), uint64(rv.Size))
		}
	}

	// .BTF
	if len(mapSyms) > 0 || len(o.Rodata) > 0 {
		w.addSection(elfSection{name: ".BTF", typ: shtProgbits, data: buildBTF(o), align: 4})
	}

	// .symtab + .strtab (added after all symbols are known).
	strtabIdx := uint32(len(w.secs)) + 1 // symtab is next, strtab after it
	symtabIdx := w.addSection(elfSection{
		name: ".symtab", typ: shtSymtab, data: w.symtab,
		link: strtabIdx, info: uint32(w.localSyms), align: 8, entsize: 24,
	})
	w.addSection(elfSection{name: ".strtab", typ: shtStrtab, data: w.symStr, align: 1})

	// A relocation section per program that references maps or rodata.
	for _, pi := range progs {
		if len(pi.relocs) == 0 {
			continue
		}
		var rel []byte
		for _, r := range pi.relocs {
			sym, ok := w.symName[r.Sym]
			if !ok {
				continue
			}
			var b [16]byte
			binary.LittleEndian.PutUint64(b[0:], uint64(r.Insn*8))         // r_offset
			binary.LittleEndian.PutUint64(b[8:], uint64(sym)<<32|rBPF6464) // r_info
			rel = append(rel, b[:]...)
		}
		w.addSection(elfSection{
			name: ".rel" + w.secs[pi.sec].name, typ: shtRel, data: rel,
			link: symtabIdx, info: pi.sec, align: 8, entsize: 16,
		})
	}

	return w.render()
}

// elfCode returns a program's bytecode with each map/rodata ld_imm64 cleared to
// a bare load, so libbpf fills in the map fd (and value offset) from the
// relocation.
func elfCode(p *Program) []byte {
	insns := append([]Insn(nil), p.Insns...)
	for _, r := range p.Relocs {
		insns[r.Insn].Src = 0
		insns[r.Insn].Imm = 0
		if r.Insn+1 < len(insns) {
			insns[r.Insn+1].Imm = 0
		}
	}
	return (&Prog{Insns: insns}).Bytes()
}

// render lays out the ELF header, section data, the section-header string table,
// and the section headers.
func (w *elfWriter) render() []byte {
	// Build the section-header string table.
	shstr := []byte{0}
	shstrOff := make([]uint32, len(w.secs)+1)
	for i := range w.secs {
		shstrOff[i] = uint32(len(shstr))
		shstr = append(shstr, w.secs[i].name...)
		shstr = append(shstr, 0)
	}
	shstrIdx := uint32(len(w.secs))
	shstrNameOff := uint32(len(shstr))
	shstr = append(shstr, ".shstrtab"...)
	shstr = append(shstr, 0)
	w.secs = append(w.secs, elfSection{name: ".shstrtab", typ: shtStrtab, data: shstr, align: 1})
	shstrOff = append(shstrOff, shstrNameOff)

	const ehSize = 64
	const shSize = 64
	// Lay out section data after the ELF header.
	off := uint64(ehSize)
	dataOff := make([]uint64, len(w.secs))
	for i := range w.secs {
		if w.secs[i].typ == 0 { // null section
			continue
		}
		a := w.secs[i].align
		if a > 1 {
			off = (off + a - 1) &^ (a - 1)
		}
		dataOff[i] = off
		off += uint64(len(w.secs[i].data))
	}
	// Section header table, 8-byte aligned.
	off = (off + 7) &^ 7
	shoff := off

	out := make([]byte, shoff+uint64(len(w.secs))*shSize)

	// ELF header.
	copy(out[0:], []byte{0x7f, 'E', 'L', 'F', 2 /*ELFCLASS64*/, 1 /*LSB*/, 1 /*version*/})
	binary.LittleEndian.PutUint16(out[16:], etRel)
	binary.LittleEndian.PutUint16(out[18:], emBPF)
	binary.LittleEndian.PutUint32(out[20:], 1) // version
	binary.LittleEndian.PutUint64(out[40:], shoff)
	binary.LittleEndian.PutUint16(out[52:], ehSize)
	binary.LittleEndian.PutUint16(out[58:], shSize)
	binary.LittleEndian.PutUint16(out[60:], uint16(len(w.secs)))
	binary.LittleEndian.PutUint16(out[62:], uint16(shstrIdx))

	// Section data and headers.
	for i := range w.secs {
		s := &w.secs[i]
		if s.typ != 0 {
			copy(out[dataOff[i]:], s.data)
		}
		h := out[shoff+uint64(i)*shSize:]
		binary.LittleEndian.PutUint32(h[0:], shstrOff[i]) // sh_name
		binary.LittleEndian.PutUint32(h[4:], s.typ)
		binary.LittleEndian.PutUint64(h[8:], uint64(s.flags))
		binary.LittleEndian.PutUint64(h[24:], dataOff[i]) // sh_offset
		if s.typ != 0 {
			binary.LittleEndian.PutUint64(h[32:], uint64(len(s.data))) // sh_size
		}
		binary.LittleEndian.PutUint32(h[40:], s.link)
		binary.LittleEndian.PutUint32(h[44:], s.info)
		if s.align > 0 {
			binary.LittleEndian.PutUint64(h[48:], s.align)
		}
		binary.LittleEndian.PutUint64(h[56:], s.entsize)
	}
	return out
}
