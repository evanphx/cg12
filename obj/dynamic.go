package obj

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// Dynamic-linking ELF constants.
const (
	etDyn = 3

	ptDynamic = 2
	ptInterp  = 3
	ptPhdr    = 6

	shtHash    = 5
	shtDynamic = 6
	shtDynsym  = 11

	dtNull     = 0
	dtNeeded   = 1
	dtPltRelSz = 2
	dtPltGot   = 3
	dtHash     = 4
	dtStrTab   = 5
	dtSymTab   = 6
	dtRela     = 7
	dtRelaSz   = 8
	dtRelaEnt  = 9
	dtStrSz    = 10
	dtSymEnt   = 11
	dtSoname   = 14
	dtPltRel   = 20
	dtJmpRel   = 23
	dtFlags    = 30
	dtFlags1   = 0x6ffffffb

	dfBindNow = 0x8 // resolve every relocation at load time (no lazy binding)
	df1Now    = 0x1

	// Relocations the dynamic loader applies. JUMP_SLOT binds a PLT's GOT slot to
	// an imported symbol; RELATIVE rebases an absolute address by the load bias.
	R_AARCH64_JUMP_SLOT = 1026
	R_AARCH64_RELATIVE  = 1027
	R_X86_64_JUMP_SLOT  = 7
	R_X86_64_RELATIVE   = 8

	// The PLT's GOT reserves three leading slots per the psABI: the address of
	// _DYNAMIC, and two the loader uses for lazy resolution (unused under BIND_NOW).
	gotReserved = 3
)

// DynOptions configures a dynamically linked image.
type DynOptions struct {
	Interp string   // dynamic-loader path, e.g. /lib/ld-linux-aarch64.so.1 (PT_INTERP)
	Needed []string // shared libraries to load, e.g. libc.so.6 (DT_NEEDED)

	// PIE emits a position-independent executable (ET_DYN linked at 0) that the
	// loader may place at any base. Absolute references then cannot be bound here
	// and become RELATIVE relocations the loader rebases.
	PIE bool

	// Export publishes these defined symbols in the dynamic symbol table, so a
	// shared library can resolve against the executable (or dlsym can find them).
	Export []string
}

// WriteDynamicExecutable links the object into a dynamically linked executable
// that calls into shared libraries. See writeDynImage for how imports are bound.
func (o *Object) WriteDynamicExecutable(entrySym string, opts DynOptions) ([]byte, error) {
	if opts.Interp == "" {
		return nil, fmt.Errorf("obj: a dynamic executable needs an interpreter path")
	}
	if entrySym == "" {
		return nil, fmt.Errorf("obj: a dynamic executable needs an entry symbol")
	}
	return o.writeDynImage(dynImage{
		entry:  entrySym,
		interp: opts.Interp,
		needed: opts.Needed,
		export: opts.Export,
		pie:    opts.PIE,
	})
}

// SharedOptions configures a shared library.
type SharedOptions struct {
	Soname string   // the name others link against, e.g. libfoo.so.1 (DT_SONAME)
	Needed []string // shared libraries this one needs (DT_NEEDED)
	Export []string // symbols to publish for others to resolve against
}

// WriteSharedLibrary links the object into a shared library: a position-
// independent ET_DYN image with no entry point and no interpreter, publishing
// Export in its dynamic symbol table so the loader (or dlsym) can find them.
func (o *Object) WriteSharedLibrary(opts SharedOptions) ([]byte, error) {
	return o.writeDynImage(dynImage{
		needed: opts.Needed,
		export: opts.Export,
		soname: opts.Soname,
		pie:    true, // a shared library is position-independent by definition
	})
}

// dynImage is the plan for one dynamically linked image.
type dynImage struct {
	entry  string   // entry symbol; empty for a shared library
	interp string   // loader path; empty for a shared library
	needed []string // DT_NEEDED
	export []string // symbols published in .dynsym
	soname string   // DT_SONAME; empty for an executable
	pie    bool     // position-independent (ET_DYN linked at 0)
}

// writeDynImage builds a dynamically linked ELF. Every symbol referenced by a
// relocation but not defined here is an import: it gets a .dynsym entry, a GOT
// slot with a JUMP_SLOT relocation for the loader to fill, and a PLT stub that
// jumps through that slot. Calls to the import are bound to the stub, so the
// backend's ordinary PC-relative call reaches the shared library's function.
// Binding is eager (DF_BIND_NOW), which keeps each stub to a single indirect jump
// and needs no lazy resolver.
//
// A non-PIE image is linked at a fixed base, so references among its own symbols
// resolve statically and only imports need the loader. A PIE is linked at 0 and
// may be placed anywhere: PC-relative references still resolve here (the distance
// is invariant), but absolute ones become RELATIVE relocations the loader rebases.
func (o *Object) writeDynImage(im dynImage) ([]byte, error) {
	stubSz := pltStubSize(o.Machine)
	if stubSz == 0 {
		return nil, fmt.Errorf("obj: cannot build a PLT for machine %d", o.Machine)
	}
	imports := o.imports()

	base := uint64(execBase)
	etype := uint16(etExec)
	if im.pie {
		base, etype = 0, etDyn
	}

	// Count the RELATIVE relocations before laying out, since .rela.dyn's size
	// feeds the layout. Only a position-independent image needs them.
	nRel := 0
	if im.pie {
		for _, r := range o.allRelocs() {
			if isAbsoluteReloc(o.Machine, r.Type) {
				nRel++
			}
		}
		if len(imports) > 0 {
			nRel++ // GOT[0] holds &_DYNAMIC, itself an absolute address
		}
	}

	// --- section layout; vaddr = base + file offset throughout ---------------
	// PT_PHDR is not decoration: the loader derives the image's load bias from it
	// (l_addr = where it actually mapped the headers - this p_vaddr). Without it
	// the bias reads as zero, which is right for a fixed-base image but sends the
	// loader dereferencing garbage in a PIE.
	nph := 4 // PT_PHDR, PT_LOAD (r-x), PT_LOAD (rw-), PT_DYNAMIC
	var interp []byte
	if im.interp != "" {
		interp = append([]byte(im.interp), 0)
		nph++ // PT_INTERP
	}

	// The dynamic symbol table lists the null symbol, then every import (undefined,
	// resolved in a needed library), then every export (defined here, published for
	// others to resolve against). Names and the table's shape are fixed now; the
	// exports' addresses are filled in once the layout below assigns them.
	exports := append([]string(nil), im.export...)
	sort.Strings(exports)
	for _, n := range exports {
		if !o.definesSym(n) {
			return nil, fmt.Errorf("obj: cannot export %q: it is not defined here", n)
		}
	}
	dynNames := append([]string{""}, imports...)
	dynNames = append(dynNames, exports...)
	nsym := len(dynNames)

	dynstr := &strtab{}
	dynstr.add("")
	symNameOff := make([]uint32, nsym)
	for i, n := range dynNames[1:] {
		symNameOff[i+1] = dynstr.add(n)
	}
	neededOff := make([]uint32, len(im.needed))
	for i, n := range im.needed {
		neededOff[i] = dynstr.add(n)
	}
	var sonameOff uint32
	if im.soname != "" {
		sonameOff = dynstr.add(im.soname)
	}
	hash := sysvHash(dynNames)

	off := alignUp(64+nph*56, 8)
	interpOff := off
	off += len(interp)
	off = alignUp(off, 8)
	hashOff := off
	off += len(hash)
	dynsymOff := off
	off += nsym * 24
	dynstrOff := off
	off += len(dynstr.b)
	off = alignUp(off, 8)
	relaDynOff := off
	off += nRel * 24
	relaPltOff := off
	off += len(imports) * 24
	off = alignUp(off, 16)
	pltOff := off
	off += len(imports) * stubSz
	off = alignUp(off, 16)
	textOff := off
	off += len(o.Text)
	roEnd := off

	// The writable segment starts on a fresh page so it never shares one with the
	// read-execute segment.
	rwOff := alignUp(roEnd, execAlign)
	gotOff := rwOff
	if len(imports) > 0 {
		off = gotOff + (gotReserved+len(imports))*8
	} else {
		off = gotOff
	}
	dynamicOff := alignUp(off, 8)
	ndyn := len(im.needed) + 8 // NEEDED..., HASH, STRTAB, SYMTAB, STRSZ, SYMENT, FLAGS, FLAGS_1, NULL
	if im.soname != "" {
		ndyn++ // SONAME
	}
	if len(imports) > 0 {
		ndyn += 4 // PLTGOT, PLTRELSZ, PLTREL, JMPREL
	}
	if nRel > 0 {
		ndyn += 3 // RELA, RELASZ, RELAENT
	}
	off = dynamicOff + ndyn*16
	dataOff := alignUp(off, 8)
	off = dataOff + len(o.Data)
	rwEnd := off

	va := func(fileOff int) uint64 { return base + uint64(fileOff) }

	// Section-header indices, assigned in the order the sections are emitted. A
	// loadable image is run from its program headers, but a shared library is also
	// *linked against*, and the static linker reads it through its sections -- so
	// the table has to be here and an exported symbol has to name a real section.
	secIdx := 0
	nextSec := func() int { i := secIdx; secIdx++; return i }
	secNull := nextSec()
	secInterp := -1
	if interp != nil {
		secInterp = nextSec()
	}
	secHash := nextSec()
	secDynsym := nextSec()
	secDynstr := nextSec()
	secRelaDyn := -1
	if nRel > 0 {
		secRelaDyn = nextSec()
	}
	secRelaPlt, secPlt, secGot := -1, -1, -1
	if len(imports) > 0 {
		secRelaPlt = nextSec()
		secPlt = nextSec()
	}
	secText := nextSec()
	if len(imports) > 0 {
		secGot = nextSec()
	}
	secDynamic := nextSec()
	secData := -1
	if len(o.Data) > 0 {
		secData = nextSec()
	}
	secShstrtab := nextSec()
	numSec := secIdx

	// --- symbol addresses ----------------------------------------------------
	// A defined symbol resolves to its link-time address; an import resolves to
	// its PLT stub, which is also the canonical address of an imported function.
	symVaddr := map[string]uint64{}
	for _, s := range o.Syms {
		switch s.Section {
		case SecText:
			symVaddr[s.Name] = va(textOff) + s.Value
		case SecData:
			symVaddr[s.Name] = va(dataOff) + s.Value
		}
	}
	for i, n := range imports {
		symVaddr[n] = va(pltOff + i*stubSz)
	}
	var entry uint64
	if im.entry != "" {
		e, ok := symVaddr[im.entry]
		if !ok {
			return nil, fmt.Errorf("obj: entry symbol %q is not defined", im.entry)
		}
		entry = e
	}

	text := append([]byte(nil), o.Text...)
	data := append([]byte(nil), o.Data...)
	dynRel, err := resolveSection(o.Machine, im.pie, text, va(textOff), o.Relocs, symVaddr)
	if err != nil {
		return nil, err
	}
	dataRel, err := resolveSection(o.Machine, im.pie, data, va(dataOff), o.DataRelocs, symVaddr)
	if err != nil {
		return nil, err
	}
	dynRel = append(dynRel, dataRel...)
	if im.pie && len(imports) > 0 {
		dynRel = append(dynRel, Reloc{
			Offset: va(gotOff), Type: relativeType(o.Machine), Addend: int64(va(dynamicOff)),
		})
	}
	if len(dynRel) != nRel {
		return nil, fmt.Errorf("obj: counted %d relative relocations, produced %d", nRel, len(dynRel))
	}

	// --- section contents ----------------------------------------------------
	dynsym := &elfBuf{}
	dynsym.pad(24) // index 0: the null symbol
	for i, n := range dynNames[1:] {
		idx := i + 1
		if idx <= len(imports) {
			// An import: undefined here, so the loader binds it in a needed library.
			dynsym.u32(symNameOff[idx])
			dynsym.u8((stbGlobal << 4) | sttFunc)
			dynsym.u8(0)
			dynsym.u16(shnUndef)
			dynsym.u64(0) // st_value
			dynsym.u64(0) // st_size
			continue
		}
		// An export: defined here, so publish its address, size, and section.
		s := o.findSym(n)
		typ, shndx := byte(sttObject), secData
		if s.Func {
			typ = sttFunc
		}
		if s.Section == SecText {
			shndx = secText
		}
		dynsym.u32(symNameOff[idx])
		dynsym.u8((stbGlobal << 4) | typ)
		dynsym.u8(0)
		dynsym.u16(uint16(shndx))
		dynsym.u64(symVaddr[n])
		dynsym.u64(s.Size)
	}

	relaDyn := &elfBuf{}
	for _, r := range dynRel {
		relaDyn.u64(r.Offset)
		relaDyn.u64(uint64(r.Type)) // symbol index 0: RELATIVE needs no symbol
		relaDyn.i64(r.Addend)
	}

	relaPlt := &elfBuf{}
	for i := range imports {
		relaPlt.u64(va(gotOff + (gotReserved+i)*8))                // r_offset: the GOT slot
		relaPlt.u64(uint64(i+1)<<32 | uint64(jumpSlot(o.Machine))) // r_info
		relaPlt.i64(0)                                             // r_addend
	}

	plt := &elfBuf{}
	for i := range imports {
		plt.bytes(pltStub(o.Machine, va(pltOff+i*stubSz), va(gotOff+(gotReserved+i)*8)))
	}

	got := &elfBuf{}
	if len(imports) > 0 {
		got.u64(va(dynamicOff)) // GOT[0] = &_DYNAMIC
		got.u64(0)              // GOT[1], GOT[2]: the loader's lazy-resolution slots
		got.u64(0)
		for range imports {
			got.u64(0) // filled by the loader from the JUMP_SLOT relocation
		}
	}

	dyn := &elfBuf{}
	dt := func(tag, val uint64) { dyn.u64(tag); dyn.u64(val) }
	for _, n := range neededOff {
		dt(dtNeeded, uint64(n))
	}
	if im.soname != "" {
		dt(dtSoname, uint64(sonameOff))
	}
	dt(dtHash, va(hashOff))
	dt(dtStrTab, va(dynstrOff))
	dt(dtSymTab, va(dynsymOff))
	dt(dtStrSz, uint64(len(dynstr.b)))
	dt(dtSymEnt, 24)
	if nRel > 0 {
		dt(dtRela, va(relaDynOff))
		dt(dtRelaSz, uint64(len(relaDyn.b)))
		dt(dtRelaEnt, 24)
	}
	if len(imports) > 0 {
		dt(dtPltGot, va(gotOff))
		dt(dtPltRelSz, uint64(len(relaPlt.b)))
		dt(dtPltRel, dtRela)
		dt(dtJmpRel, va(relaPltOff))
	}
	dt(dtFlags, dfBindNow)
	dt(dtFlags1, df1Now)
	dt(dtNull, 0)
	if n := len(dyn.b) / 16; n != ndyn {
		return nil, fmt.Errorf("obj: dynamic section has %d entries, reserved %d", n, ndyn)
	}

	// --- emit ----------------------------------------------------------------
	out := &elfBuf{}
	out.pad(64) // ELF header, filled in at the end
	phdr := func(typ, flags uint32, off, vaddr, size, align uint64) {
		out.u32(typ)
		out.u32(flags)
		out.u64(off)
		out.u64(vaddr)
		out.u64(vaddr) // p_paddr
		out.u64(size)  // p_filesz
		out.u64(size)  // p_memsz
		out.u64(align)
	}
	// PT_PHDR must precede every loadable segment, and PT_INTERP conventionally
	// follows it. Both live inside the read-execute PT_LOAD that starts the image.
	phdr(ptPhdr, pfR, 64, va(64), uint64(nph*56), 8)
	if interp != nil {
		phdr(ptInterp, pfR, uint64(interpOff), va(interpOff), uint64(len(interp)), 1)
	}
	phdr(ptLoad, pfR|pfX, 0, base, uint64(roEnd), execAlign)
	phdr(ptLoad, pfR|pfW, uint64(rwOff), va(rwOff), uint64(rwEnd-rwOff), execAlign)
	phdr(ptDynamic, pfR|pfW, uint64(dynamicOff), va(dynamicOff), uint64(ndyn*16), 8)

	put := func(fileOff int, b []byte) {
		for len(out.b) < fileOff {
			out.u8(0)
		}
		out.bytes(b)
	}
	if interp != nil {
		put(interpOff, interp)
	}
	put(hashOff, hash)
	put(dynsymOff, dynsym.b)
	put(dynstrOff, dynstr.b)
	put(relaDynOff, relaDyn.b)
	put(relaPltOff, relaPlt.b)
	put(pltOff, plt.b)
	put(textOff, text)
	put(gotOff, got.b)
	put(dynamicOff, dyn.b)
	put(dataOff, data)

	// Section headers, so the static linker can link against this image.
	shstr := &strtab{}
	secs := make([]dsection, numSec)
	set := func(i int, name string, typ uint32, flags uint64, off int, size int, link, info int, align, entsize uint64) {
		if i < 0 {
			return
		}
		secs[i] = dsection{
			name: shstr.add(name), typ: typ, flags: flags, addr: va(off),
			off: uint64(off), size: uint64(size), link: uint32(link), info: uint32(info),
			align: align, entsize: entsize,
		}
	}
	secs[secNull] = dsection{name: shstr.add("")}
	set(secInterp, ".interp", shtProgbits, shfAlloc, interpOff, len(interp), 0, 0, 1, 0)
	set(secHash, ".hash", shtHash, shfAlloc, hashOff, len(hash), secDynsym, 0, 8, 4)
	set(secDynsym, ".dynsym", shtDynsym, shfAlloc, dynsymOff, len(dynsym.b), secDynstr, 1, 8, 24)
	set(secDynstr, ".dynstr", shtStrtab, shfAlloc, dynstrOff, len(dynstr.b), 0, 0, 1, 0)
	set(secRelaDyn, ".rela.dyn", shtRela, shfAlloc, relaDynOff, len(relaDyn.b), secDynsym, 0, 8, 24)
	set(secRelaPlt, ".rela.plt", shtRela, shfAlloc, relaPltOff, len(relaPlt.b), secDynsym, secPlt, 8, 24)
	set(secPlt, ".plt", shtProgbits, shfAlloc|shfExecinstr, pltOff, len(plt.b), 0, 0, 16, 0)
	set(secText, ".text", shtProgbits, shfAlloc|shfExecinstr, textOff, len(text), 0, 0, 16, 0)
	set(secGot, ".got.plt", shtProgbits, shfAlloc|shfWrite, gotOff, len(got.b), 0, 0, 8, 8)
	set(secDynamic, ".dynamic", shtDynamic, shfAlloc|shfWrite, dynamicOff, ndyn*16, secDynstr, 0, 8, 16)
	set(secData, ".data", shtProgbits, shfAlloc|shfWrite, dataOff, len(data), 0, 0, 8, 0)

	// .shstrtab is not loaded, so it goes after the image's mapped content.
	shstrOff := len(out.b)
	secs[secShstrtab] = dsection{
		name: shstr.add(".shstrtab"), typ: shtStrtab, off: uint64(shstrOff), align: 1,
	}
	secs[secShstrtab].size = uint64(len(shstr.b))
	out.bytes(shstr.b)

	out.align(8)
	shoff := len(out.b)
	for _, s := range secs {
		out.u32(s.name)
		out.u32(s.typ)
		out.u64(s.flags)
		out.u64(s.addr)
		out.u64(s.off)
		out.u64(s.size)
		out.u32(s.link)
		out.u32(s.info)
		out.u64(s.align)
		out.u64(s.entsize)
	}

	writeElfHeader(out.b, etype, o.Machine, entry, nph)
	writeSectionInfo(out.b, uint64(shoff), numSec, secShstrtab)
	return out.b, nil
}

// dsection is one section header of a loadable image.
type dsection struct {
	name           uint32
	typ            uint32
	flags          uint64
	addr, off      uint64
	size           uint64
	link, info     uint32
	align, entsize uint64
}

// allRelocs returns every relocation the object carries against .text and .data.
func (o *Object) allRelocs() []Reloc {
	return append(append([]Reloc{}, o.Relocs...), o.DataRelocs...)
}

// imports returns the sorted names of every symbol a relocation references but
// the object does not define -- the symbols a shared library must supply.
func (o *Object) imports() []string {
	defined := map[string]bool{}
	for _, s := range o.Syms {
		if s.Section != SecUndef {
			defined[s.Name] = true
		}
	}
	seen := map[string]bool{}
	var names []string
	for _, r := range o.allRelocs() {
		if !defined[r.Sym] && !seen[r.Sym] {
			seen[r.Sym] = true
			names = append(names, r.Sym)
		}
	}
	sort.Strings(names)
	return names
}

// resolveSection patches a section's relocations. In a position-independent image
// an absolute reference cannot be bound at link time -- the load base is unknown --
// so it becomes a RELATIVE relocation the loader applies as *slot = base + addend.
// PC-relative references always resolve here: the distance between two points in
// the image does not change when the image moves.
func resolveSection(machine uint16, pie bool, sec []byte, secVaddr uint64, relocs []Reloc, symVaddr map[string]uint64) ([]Reloc, error) {
	var dyn []Reloc
	for _, r := range relocs {
		s, ok := symVaddr[r.Sym]
		if !ok {
			return nil, fmt.Errorf("obj: undefined symbol %q referenced by relocation", r.Sym)
		}
		target := int64(s) + r.Addend
		place := int64(secVaddr) + int64(r.Offset)
		if pie {
			switch {
			case isAbsoluteReloc(machine, r.Type):
				binary.LittleEndian.PutUint64(sec[r.Offset:], uint64(target))
				dyn = append(dyn, Reloc{Offset: uint64(place), Type: relativeType(machine), Addend: target})
				continue
			case r.Type == R_X86_64_32 || r.Type == R_X86_64_32S:
				return nil, fmt.Errorf("obj: %q needs a 32-bit absolute address, which a position-independent image cannot have", r.Sym)
			}
		}
		var err error
		switch machine {
		case EM_AARCH64:
			err = resolveAArch64(sec, r, target, place)
		case EM_X86_64:
			err = resolveX86(sec, r, target, place)
		default:
			return nil, fmt.Errorf("obj: cannot link machine %d", machine)
		}
		if err != nil {
			return nil, err
		}
	}
	return dyn, nil
}

// isAbsoluteReloc reports whether a relocation stores a full 64-bit address, the
// only kind a position-independent image must defer to the loader.
func isAbsoluteReloc(machine uint16, typ uint32) bool {
	switch machine {
	case EM_AARCH64:
		return typ == R_AARCH64_ABS64
	case EM_X86_64:
		return typ == R_X86_64_64
	}
	return false
}

// relativeType is the machine's load-base rebase relocation.
func relativeType(machine uint16) uint32 {
	switch machine {
	case EM_AARCH64:
		return R_AARCH64_RELATIVE
	case EM_X86_64:
		return R_X86_64_RELATIVE
	}
	return 0
}

// sysvHash builds the SysV hash table (DT_HASH) over a dynamic symbol table,
// names[i] naming symbol i (names[0] is the null symbol). A lookup hashes the
// name to a bucket and walks the chain from there, so the table must really work
// whenever this image exports anything.
func sysvHash(names []string) []byte {
	nsym := len(names)
	nbucket := nsym/2 + 1
	buckets := make([]uint32, nbucket)
	chain := make([]uint32, nsym)
	for i := 1; i < nsym; i++ {
		b := elfHash(names[i]) % uint32(nbucket)
		chain[i] = buckets[b] // link to whatever was at the head
		buckets[b] = uint32(i)
	}
	w := &elfBuf{}
	w.u32(uint32(nbucket))
	w.u32(uint32(nsym)) // nchain: also how the loader sizes .dynsym
	for _, v := range buckets {
		w.u32(v)
	}
	for _, v := range chain {
		w.u32(v)
	}
	return w.b
}

// elfHash is the SysV symbol hash from the ELF specification.
func elfHash(name string) uint32 {
	var h, g uint32
	for i := 0; i < len(name); i++ {
		h = (h << 4) + uint32(name[i])
		g = h & 0xf0000000
		if g != 0 {
			h ^= g >> 24
		}
		h &^= g
	}
	return h
}

// definesSym reports whether the object defines name.
func (o *Object) definesSym(name string) bool {
	for _, s := range o.Syms {
		if s.Name == name && s.Section != SecUndef {
			return true
		}
	}
	return false
}

// findSym returns the object's definition of name (empty if absent).
func (o *Object) findSym(name string) Sym {
	for _, s := range o.Syms {
		if s.Name == name && s.Section != SecUndef {
			return s
		}
	}
	return Sym{}
}

// jumpSlot is the machine's PLT-slot relocation type.
func jumpSlot(machine uint16) uint32 {
	switch machine {
	case EM_AARCH64:
		return R_AARCH64_JUMP_SLOT
	case EM_X86_64:
		return R_X86_64_JUMP_SLOT
	}
	return 0
}

// pltStubSize is the byte size of one PLT stub (padded to a uniform stride).
func pltStubSize(machine uint16) int {
	switch machine {
	case EM_AARCH64, EM_X86_64:
		return 16
	}
	return 0
}

// pltStub encodes a stub that jumps to the address the loader stored in the GOT
// slot at gotVaddr. Under eager binding the slot always holds the real target, so
// a single indirect jump suffices. Both forms address the GOT PC-relatively, so
// the stub works unchanged wherever the image is loaded.
func pltStub(machine uint16, stubVaddr, gotVaddr uint64) []byte {
	b := &elfBuf{}
	switch machine {
	case EM_AARCH64:
		// adrp x16, page(got) ; ldr x16, [x16, #lo12(got)] ; br x16
		page := ((int64(gotVaddr) &^ 0xfff) - (int64(stubVaddr) &^ 0xfff)) >> 12
		b.u32(0x90000010 | uint32(page&3)<<29 | uint32((page>>2)&0x7ffff)<<5)
		b.u32(0xf9400210 | uint32((gotVaddr&0xfff)/8)<<10)
		b.u32(0xd61f0200) // br x16
		b.u32(0xd503201f) // nop, padding to the 16-byte stride
	case EM_X86_64:
		// jmp *disp32(%rip), relative to the end of the 6-byte instruction.
		b.bytes([]byte{0xff, 0x25})
		b.u32(uint32(int32(int64(gotVaddr) - int64(stubVaddr+6))))
		for i := 0; i < 10; i++ {
			b.u8(0xcc) // int3 padding to the 16-byte stride
		}
	}
	return b.b
}
