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
	rBPF6464    = 1  // 64-bit ld_imm64 data reference (map / rodata)
	rBPF6432    = 10 // 32-bit call reference (BPF-to-BPF)
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

// secReloc collects one output section's relocations while symbols are still
// being assigned; they are encoded once every symbol index is known.
type secReloc struct {
	offset uint64 // byte offset within the section
	sym    string // referenced symbol name
	typ    uint32 // rBPF6464 (data) or rBPF6432 (call)
}

// ELF renders the object as a relocatable eBPF ELF object file, in the layout
// libbpf expects: each entry program in its own section, all subprograms in one
// .text section, FUNC symbols for every function, and R_BPF_64_64 / R_BPF_64_32
// relocations for map/rodata references and BPF-to-BPF calls.
func (o *Object) ELF() []byte {
	w := newELFWriter()
	w.addSection(elfSection{name: ""}) // section 0 is null

	// The section a function lands in and its byte offset there.
	type placed struct {
		sec uint32
		off uint64
	}
	loc := map[string]placed{}
	relBySec := map[uint32][]secReloc{}

	// One PROGBITS section per entry program; subprograms are gathered into .text.
	var textFuncs []CompiledFunc
	for _, f := range o.Funcs {
		if f.Section == "" {
			textFuncs = append(textFuncs, f)
			continue
		}
		code := elfFuncCode(f)
		sec := w.addSection(elfSection{name: f.Section, typ: shtProgbits, flags: shfAlloc | shfExec, data: code, align: 8})
		loc[f.Name] = placed{sec, 0}
		relBySec[sec] = funcRelocs(f, 0)
	}
	if len(textFuncs) > 0 {
		var text []byte
		var rels []secReloc
		for _, f := range textFuncs {
			off := uint64(len(text))
			loc[f.Name] = placed{0, off} // section filled in below
			rels = append(rels, funcRelocs(f, off)...)
			text = append(text, elfFuncCode(f)...)
		}
		sec := w.addSection(elfSection{name: ".text", typ: shtProgbits, flags: shfAlloc | shfExec, data: text, align: 8})
		for i := range textFuncs {
			p := loc[textFuncs[i].Name]
			p.sec = sec
			loc[textFuncs[i].Name] = p
		}
		relBySec[sec] = rels
	}

	if o.License != "" {
		lic := append([]byte(o.License), 0)
		w.addSection(elfSection{name: "license", typ: shtProgbits, flags: shfAlloc, data: lic, align: 1})
	}

	var mapSyms []MapDef
	for _, m := range o.Maps {
		if m.Name != rodataName {
			mapSyms = append(mapSyms, m)
		}
	}
	mapsSec := uint32(0)
	if len(mapSyms) > 0 {
		mapsSec = w.addSection(elfSection{name: ".maps", typ: shtProgbits, flags: shfAlloc, data: make([]byte, len(mapSyms)*32), align: 8})
	}
	rodataSec := uint32(0)
	for _, m := range o.Maps {
		if m.Name == rodataName {
			rodataSec = w.addSection(elfSection{name: rodataName, typ: shtProgbits, flags: shfAlloc, data: m.Initial, align: 8})
		}
	}
	if len(mapSyms) > 0 || len(o.Rodata) > 0 {
		w.addSection(elfSection{name: ".BTF", typ: shtProgbits, data: buildBTF(o), align: 4})
	}

	// Symbols: a FUNC per function, an OBJECT per map and rodata global.
	for _, f := range o.Funcs {
		p := loc[f.Name]
		w.addSym(f.Name, sttFunc, p.sec, p.off, uint64(len(f.Insns)*8))
	}
	for i, m := range mapSyms {
		w.addSym(m.Name, sttObject, mapsSec, uint64(i*32), 32)
	}
	for _, rv := range o.Rodata {
		w.addSym(rv.Name, sttObject, rodataSec, uint64(rv.Off), uint64(rv.Size))
	}

	strtabIdx := uint32(len(w.secs)) + 1
	symtabIdx := w.addSection(elfSection{
		name: ".symtab", typ: shtSymtab, data: w.symtab,
		link: strtabIdx, info: uint32(w.localSyms), align: 8, entsize: 24,
	})
	w.addSection(elfSection{name: ".strtab", typ: shtStrtab, data: w.symStr, align: 1})

	// A relocation section per code section that has references (in section order
	// for a deterministic file).
	nCode := uint32(len(w.secs))
	for sec := uint32(0); sec < nCode; sec++ {
		rels := relBySec[sec]
		if len(rels) == 0 {
			continue
		}
		var data []byte
		for _, r := range rels {
			sym, ok := w.symName[r.sym]
			if !ok {
				continue
			}
			var b [16]byte
			binary.LittleEndian.PutUint64(b[0:], r.offset)
			binary.LittleEndian.PutUint64(b[8:], uint64(sym)<<32|uint64(r.typ))
			data = append(data, b[:]...)
		}
		w.addSection(elfSection{
			name: ".rel" + w.secs[sec].name, typ: shtRel, data: data,
			link: symtabIdx, info: sec, align: 8, entsize: 16,
		})
	}
	return w.render()
}

// elfFuncCode returns a function's bytecode with map/rodata loads and BPF-to-BPF
// calls left as bare placeholders for libbpf to fill from the relocations.
func elfFuncCode(f CompiledFunc) []byte {
	insns := append([]Insn(nil), f.Insns...)
	for _, r := range f.MapRelocs {
		insns[r.Insn].Src = 0
		insns[r.Insn].Imm = 0
		if r.Insn+1 < len(insns) {
			insns[r.Insn+1].Imm = 0
		}
	}
	for _, r := range f.CallRelocs {
		insns[r.Insn].Imm = -1 // src stays BPF_PSEUDO_CALL; the reloc supplies the target
	}
	return (&Prog{Insns: insns}).Bytes()
}

// funcRelocs returns a function's relocations at their byte offsets within a
// section that begins at baseInsn instructions (base bytes = baseInsn*8... here
// base is a byte offset already).
func funcRelocs(f CompiledFunc, base uint64) []secReloc {
	var out []secReloc
	for _, r := range f.MapRelocs {
		out = append(out, secReloc{offset: base + uint64(r.Insn)*8, sym: r.Sym, typ: rBPF6464})
	}
	for _, r := range f.CallRelocs {
		out = append(out, secReloc{offset: base + uint64(r.Insn)*8, sym: r.Func, typ: rBPF6432})
	}
	return out
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
