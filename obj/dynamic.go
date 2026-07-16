package obj

import (
	"fmt"
	"sort"
)

// Dynamic-linking ELF constants.
const (
	ptDynamic = 2
	ptInterp  = 3

	dtNull     = 0
	dtNeeded   = 1
	dtPltRelSz = 2
	dtPltGot   = 3
	dtHash     = 4
	dtStrTab   = 5
	dtSymTab   = 6
	dtRela     = 7
	dtStrSz    = 10
	dtSymEnt   = 11
	dtPltRel   = 20
	dtJmpRel   = 23
	dtFlags    = 30
	dtFlags1   = 0x6ffffffb

	dfBindNow = 0x8 // resolve every relocation at load time (no lazy binding)
	df1Now    = 0x1

	// Relocations the dynamic loader applies to a PLT's GOT slot.
	R_AARCH64_JUMP_SLOT = 1026
	R_X86_64_JUMP_SLOT  = 7

	// The PLT's GOT reserves three leading slots per the psABI: the address of
	// _DYNAMIC, and two the loader uses for lazy resolution (unused under BIND_NOW).
	gotReserved = 3
)

// DynOptions configures a dynamically linked executable.
type DynOptions struct {
	Interp string   // dynamic-loader path, e.g. /lib/ld-linux-aarch64.so.1 (PT_INTERP)
	Needed []string // shared libraries to load, e.g. libc.so.6 (DT_NEEDED)
}

// WriteDynamicExecutable links the object into a dynamically linked ET_EXEC that
// calls into shared libraries. Every symbol referenced by a relocation but not
// defined here is treated as an import: it gets a .dynsym entry, a GOT slot with
// a JUMP_SLOT relocation for the loader to fill, and a PLT stub that jumps through
// that slot. Calls to the import are then bound to the stub, so the backend's
// ordinary PC-relative call reaches the shared library's function. Binding is
// eager (DF_BIND_NOW), which keeps the PLT to a single indirect jump and needs no
// lazy resolver.
//
// The image is non-PIE at a fixed base, so references among its own symbols are
// resolved statically here and only the imports need the loader.
func (o *Object) WriteDynamicExecutable(entrySym string, opts DynOptions) ([]byte, error) {
	if opts.Interp == "" {
		return nil, fmt.Errorf("obj: a dynamic executable needs an interpreter path")
	}
	stubSz := pltStubSize(o.Machine)
	if stubSz == 0 {
		return nil, fmt.Errorf("obj: cannot build a PLT for machine %d", o.Machine)
	}
	imports := o.imports()

	// --- section layout; vaddr = execBase + file offset throughout -----------
	nph := 4 // PT_LOAD (r-x), PT_LOAD (rw-), PT_INTERP, PT_DYNAMIC
	interp := append([]byte(opts.Interp), 0)

	// .dynstr holds the import names and the needed-library names.
	dynstr := &strtab{}
	dynstr.add("")
	symNameOff := make([]uint32, len(imports))
	for i, n := range imports {
		symNameOff[i] = dynstr.add(n)
	}
	neededOff := make([]uint32, len(opts.Needed))
	for i, n := range opts.Needed {
		neededOff[i] = dynstr.add(n)
	}
	nsym := len(imports) + 1 // index 0 is the null symbol
	hash := sysvHash(nsym)

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
	gotOff := alignUp(roEnd, execAlign)
	off = gotOff + (gotReserved+len(imports))*8
	dynamicOff := alignUp(off, 8)
	ndyn := len(opts.Needed) + 12
	off = dynamicOff + ndyn*16
	dataOff := alignUp(off, 8)
	off = dataOff + len(o.Data)
	rwEnd := off

	va := func(fileOff int) uint64 { return uint64(execBase + fileOff) }

	// --- symbol addresses ----------------------------------------------------
	// A defined symbol resolves to its final address; an import resolves to its
	// PLT stub, which is also the canonical address of an imported function.
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
	entry, ok := symVaddr[entrySym]
	if !ok {
		return nil, fmt.Errorf("obj: entry symbol %q is not defined", entrySym)
	}

	text := append([]byte(nil), o.Text...)
	data := append([]byte(nil), o.Data...)
	if err := resolveRelocs(o.Machine, text, va(textOff), o.Relocs, symVaddr); err != nil {
		return nil, err
	}
	if err := resolveRelocs(o.Machine, data, va(dataOff), o.DataRelocs, symVaddr); err != nil {
		return nil, err
	}

	// --- section contents ----------------------------------------------------
	dynsym := &elfBuf{}
	dynsym.pad(24) // index 0: the null symbol
	for i := range imports {
		dynsym.u32(symNameOff[i])
		dynsym.u8((stbGlobal << 4) | sttFunc)
		dynsym.u8(0)         // st_other
		dynsym.u16(shnUndef) // undefined: the loader finds it in a needed library
		dynsym.u64(0)        // st_value
		dynsym.u64(0)        // st_size
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
	got.u64(va(dynamicOff)) // GOT[0] = &_DYNAMIC
	got.u64(0)              // GOT[1], GOT[2]: the loader's lazy-resolution slots
	got.u64(0)
	for range imports {
		got.u64(0) // filled by the loader from the JUMP_SLOT relocation
	}

	dyn := &elfBuf{}
	dt := func(tag, val uint64) { dyn.u64(tag); dyn.u64(val) }
	for _, n := range neededOff {
		dt(dtNeeded, uint64(n))
	}
	dt(dtHash, va(hashOff))
	dt(dtStrTab, va(dynstrOff))
	dt(dtSymTab, va(dynsymOff))
	dt(dtStrSz, uint64(len(dynstr.b)))
	dt(dtSymEnt, 24)
	dt(dtPltGot, va(gotOff))
	dt(dtPltRelSz, uint64(len(relaPlt.b)))
	dt(dtPltRel, dtRela)
	dt(dtJmpRel, va(relaPltOff))
	dt(dtFlags, dfBindNow)
	dt(dtFlags1, df1Now)
	dt(dtNull, 0)
	if got := len(dyn.b) / 16; got != ndyn {
		return nil, fmt.Errorf("obj: dynamic section has %d entries, reserved %d", got, ndyn)
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
	phdr(ptLoad, pfR|pfX, 0, execBase, uint64(roEnd), execAlign)
	phdr(ptLoad, pfR|pfW, uint64(gotOff), va(gotOff), uint64(rwEnd-gotOff), execAlign)
	phdr(ptInterp, pfR, uint64(interpOff), va(interpOff), uint64(len(interp)), 1)
	phdr(ptDynamic, pfR|pfW, uint64(dynamicOff), va(dynamicOff), uint64(ndyn*16), 8)

	put := func(fileOff int, b []byte) {
		for len(out.b) < fileOff {
			out.u8(0)
		}
		out.bytes(b)
	}
	put(interpOff, interp)
	put(hashOff, hash)
	put(dynsymOff, dynsym.b)
	put(dynstrOff, dynstr.b)
	put(relaPltOff, relaPlt.b)
	put(pltOff, plt.b)
	put(textOff, text)
	put(gotOff, got.b)
	put(dynamicOff, dyn.b)
	put(dataOff, data)

	writeExecHeader(out.b, o.Machine, entry, nph)
	return out.b, nil
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
	for _, r := range append(append([]Reloc{}, o.Relocs...), o.DataRelocs...) {
		if !defined[r.Sym] && !seen[r.Sym] {
			seen[r.Sym] = true
			names = append(names, r.Sym)
		}
	}
	sort.Strings(names)
	return names
}

// sysvHash builds a structurally valid SysV hash table (DT_HASH) covering nsym
// symbols. The loader reads nchain to size the symbol table; the single bucket is
// empty because this image exports nothing for others to look up -- its own
// imports are resolved in the libraries' hash tables, not this one.
func sysvHash(nsym int) []byte {
	b := &elfBuf{}
	b.u32(1)            // nbucket
	b.u32(uint32(nsym)) // nchain
	b.u32(0)            // bucket[0] = STN_UNDEF: no exported symbol
	for i := 0; i < nsym; i++ {
		b.u32(0) // chain[i]
	}
	return b.b
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
// a single indirect jump suffices.
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
