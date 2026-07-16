package obj

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
)

// Executable layout constants. The image is a static, non-PIE ET_EXEC loaded at a
// fixed base; every byte's virtual address is execBase + its file offset, so with
// execBase a multiple of execAlign the segment-alignment rule (p_vaddr ≡ p_offset
// mod p_align) holds for free.
const (
	execBase  = 0x400000 // load address of the ELF header
	execAlign = 0x10000  // segment alignment (>= the page size on arm64 and amd64)

	ptLoad = 1
	etExec = 2
	pfX    = 1
	pfW    = 2
	pfR    = 4
)

// WriteExecutable fully links the object into a static ET_EXEC ELF: it assigns
// virtual addresses to .text and .data, resolves every relocation against those
// addresses (erroring on any undefined symbol), and emits program headers so the
// kernel can load and run it directly -- no external linker or C runtime. entrySym
// names the symbol the ELF entry point (e_entry) points at (e.g. "_start").
func (o *Object) WriteExecutable(entrySym string) ([]byte, error) {
	nph := 3 // PT_LOAD (r-x), PT_NOTE, PT_GNU_STACK
	if len(o.Data) > 0 {
		nph++ // PT_LOAD (rw-)
	}
	note, noteDescOff := buildIDNote()
	noteOff := alignUp(64+nph*56, 4)
	textOff := alignUp(noteOff+len(note), 16)
	textVaddr := uint64(execBase + textOff)

	dataOff := 0
	var dataVaddr uint64
	if len(o.Data) > 0 {
		dataOff = alignUp(textOff+len(o.Text), execAlign)
		dataVaddr = uint64(execBase + dataOff)
	}

	// Final virtual address of every defined symbol.
	symVaddr := map[string]uint64{}
	for _, s := range o.Syms {
		switch s.Section {
		case SecText:
			symVaddr[s.Name] = textVaddr + s.Value
		case SecData:
			symVaddr[s.Name] = dataVaddr + s.Value
		}
	}
	entry, ok := symVaddr[entrySym]
	if !ok {
		return nil, fmt.Errorf("obj: entry symbol %q is not defined", entrySym)
	}

	// Resolve relocations into fresh copies of the sections.
	text := append([]byte(nil), o.Text...)
	data := append([]byte(nil), o.Data...)
	if err := resolveRelocs(o.Machine, text, textVaddr, o.Relocs, symVaddr); err != nil {
		return nil, err
	}
	if err := resolveRelocs(o.Machine, data, dataVaddr, o.DataRelocs, symVaddr); err != nil {
		return nil, err
	}

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
	textFilesz := uint64(textOff + len(text))
	phdr(ptLoad, pfR|pfX, 0, execBase, textFilesz, execAlign)
	if len(o.Data) > 0 {
		phdr(ptLoad, pfR|pfW, uint64(dataOff), dataVaddr, uint64(len(data)), execAlign)
	}
	phdr(ptNote, pfR, uint64(noteOff), uint64(execBase+noteOff), uint64(len(note)), 4)
	// An empty segment whose flags say the stack wants no execute permission. Its
	// absence is not neutral: the kernel then falls back to an executable stack.
	phdr(ptGnuStack, pfR|pfW, 0, 0, 0, 0x10)

	put := func(fileOff int, b []byte) {
		for len(out.b) < fileOff {
			out.u8(0)
		}
		out.bytes(b)
	}
	put(noteOff, note)
	put(textOff, text)
	if len(o.Data) > 0 {
		put(dataOff, data)
	}

	writeExecHeader(out.b, o.Machine, entry, nph)
	setBuildID(out.b, noteOff+noteDescOff)
	return out.b, nil
}

// resolveRelocs patches every relocation in one section (at virtual address
// secVaddr) to the final address of its target symbol.
func resolveRelocs(machine uint16, sec []byte, secVaddr uint64, relocs []Reloc, symVaddr map[string]uint64) error {
	for _, r := range relocs {
		if isTLSReloc(r.Type) || isX86TLSReloc(r.Type) {
			return fmt.Errorf("obj: %q is thread-local, which a static executable cannot use: "+
				"nothing would set up the thread pointer. Link it dynamically, where the loader does", r.Sym)
		}
		s, ok := symVaddr[r.Sym]
		if !ok {
			return fmt.Errorf("obj: undefined symbol %q referenced by relocation", r.Sym)
		}
		target := int64(s) + r.Addend
		place := int64(secVaddr) + int64(r.Offset)
		var err error
		switch machine {
		case EM_AARCH64:
			err = resolveAArch64(sec, r, target, place)
		case EM_X86_64:
			err = resolveX86(sec, r, target, place)
		default:
			return fmt.Errorf("obj: cannot link machine %d", machine)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// resolveAArch64 applies one AArch64 relocation: absolute (ABS64), PC-relative
// branch (CALL26/JUMP26), or symbol-address (ADRP page + ADD low-12).
func resolveAArch64(sec []byte, r Reloc, target, place int64) error {
	switch r.Type {
	case R_AARCH64_ABS64:
		binary.LittleEndian.PutUint64(sec[r.Offset:], uint64(target))
	case R_AARCH64_CALL26, R_AARCH64_JUMP26:
		disp := target - place
		if disp%4 != 0 || disp < -(1<<27) || disp >= (1<<27) {
			return fmt.Errorf("obj: branch to %q out of range (%d bytes)", r.Sym, disp)
		}
		w := binary.LittleEndian.Uint32(sec[r.Offset:])
		w = (w & 0xfc000000) | uint32((disp>>2)&0x03ffffff)
		binary.LittleEndian.PutUint32(sec[r.Offset:], w)
	case R_AARCH64_ADR_PREL_PG_HI21:
		page := ((target &^ 0xfff) - (place &^ 0xfff)) >> 12
		if page < -(1<<20) || page >= (1<<20) {
			return fmt.Errorf("obj: adrp to %q out of range", r.Sym)
		}
		w := binary.LittleEndian.Uint32(sec[r.Offset:])
		immlo := uint32(page & 3)
		immhi := uint32((page >> 2) & 0x7ffff)
		w = (w &^ ((3 << 29) | (0x7ffff << 5))) | (immlo << 29) | (immhi << 5)
		binary.LittleEndian.PutUint32(sec[r.Offset:], w)
	case R_AARCH64_ADD_ABS_LO12_NC:
		w := binary.LittleEndian.Uint32(sec[r.Offset:])
		w = (w &^ (0xfff << 10)) | (uint32(target&0xfff) << 10)
		binary.LittleEndian.PutUint32(sec[r.Offset:], w)
	default:
		return fmt.Errorf("obj: cannot statically resolve aarch64 relocation type %d (symbol %q)", r.Type, r.Sym)
	}
	return nil
}

// resolveX86 applies one x86-64 relocation: absolute (64/32/32S) or PC-relative
// (PC32/PLT32, whose addend already folds in the instruction-end bias).
func resolveX86(sec []byte, r Reloc, target, place int64) error {
	switch r.Type {
	case R_X86_64_64:
		binary.LittleEndian.PutUint64(sec[r.Offset:], uint64(target))
	case R_X86_64_PC32, R_X86_64_PLT32:
		disp := target - place
		if disp < math.MinInt32 || disp > math.MaxInt32 {
			return fmt.Errorf("obj: reference to %q out of range (%d bytes)", r.Sym, disp)
		}
		binary.LittleEndian.PutUint32(sec[r.Offset:], uint32(int32(disp)))
	case R_X86_64_32, R_X86_64_32S:
		binary.LittleEndian.PutUint32(sec[r.Offset:], uint32(int32(target)))
	default:
		return fmt.Errorf("obj: cannot statically resolve x86-64 relocation type %d (symbol %q)", r.Type, r.Sym)
	}
	return nil
}

// writeExecHeader fills the 64-byte ELF header of a static executable in place.
func writeExecHeader(b []byte, machine uint16, entry uint64, phnum int) {
	writeElfHeader(b, etExec, machine, entry, phnum)
}

// writeElfHeader fills the 64-byte ELF header of a loadable image (ET_EXEC or
// ET_DYN) in place. Such an image is loaded from its program headers, so it needs
// no section headers.
func writeElfHeader(b []byte, etype, machine uint16, entry uint64, phnum int) {
	h := &elfBuf{b: b[:0:64]}
	h.bytes([]byte{0x7f, 'E', 'L', 'F', elfclass64, elfdata2lsb, evCurrent, 0})
	h.pad(8) // rest of e_ident
	h.u16(etype)
	h.u16(machine)
	h.u32(evCurrent)
	h.u64(entry) // e_entry
	h.u64(64)    // e_phoff (program headers follow the ELF header)
	h.u64(0)     // e_shoff (no section headers -- not needed to load and run)
	h.u32(0)     // e_flags
	h.u16(64)    // e_ehsize
	h.u16(56)    // e_phentsize
	h.u16(uint16(phnum))
	h.u16(0) // e_shentsize
	h.u16(0) // e_shnum
	h.u16(0) // e_shstrndx
}

// writeSectionInfo patches an already-written ELF header to point at a section
// header table. writeElfHeader leaves these zero, which is all a loadable image
// needs; a shared library also wants them so the static linker can read it.
func writeSectionInfo(b []byte, shoff uint64, shnum, shstrndx int) {
	binary.LittleEndian.PutUint64(b[40:], shoff)         // e_shoff
	binary.LittleEndian.PutUint16(b[58:], 64)            // e_shentsize
	binary.LittleEndian.PutUint16(b[60:], uint16(shnum)) // e_shnum
	binary.LittleEndian.PutUint16(b[62:], uint16(shstrndx))
}

// buildIDSize is the length of the identifier, matching the 20 bytes a linker's
// default (sha1-shaped) build ID occupies.
const buildIDSize = 20

// ntGnuBuildID is the note type naming an image's unique build identifier.
const ntGnuBuildID = 3

// buildIDNote returns a .note.gnu.build-id note with the identifier still zeroed,
// and the offset of that identifier within the note. The value cannot be known
// until the image is finished -- it is a hash *of* the image -- so it is patched
// in afterwards by setBuildID.
func buildIDNote() (note []byte, descOff int) {
	b := &elfBuf{}
	b.u32(4)            // n_namesz: "GNU\0"
	b.u32(buildIDSize)  // n_descsz
	b.u32(ntGnuBuildID) // n_type
	b.bytes([]byte("GNU\x00"))
	descOff = len(b.b)
	b.pad(buildIDSize)
	return b.b, descOff
}

// setBuildID hashes the finished image and writes the digest into its build-id
// note. The identifier reads as zero while hashing, so the same image always
// yields the same identifier -- which is the point: it names this exact build, and
// lets a debugger match it to its symbols.
func setBuildID(image []byte, descAt int) {
	sum := sha256.Sum256(image)
	copy(image[descAt:descAt+buildIDSize], sum[:buildIDSize])
}

// alignUp rounds n up to the next multiple of a.
func alignUp(n, a int) int {
	if a < 1 {
		return n
	}
	return (n + a - 1) &^ (a - 1)
}
