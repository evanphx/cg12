package obj

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Dynamic-linking ELF constants.
const (
	etDyn = 3

	ptDynamic  = 2
	ptInterp   = 3
	ptNote     = 4
	ptTls      = 7
	ptPhdr     = 6
	ptGnuStack = 0x6474e551
	ptGnuRelro = 0x6474e552

	shtHash        = 5
	shtDynamic     = 6
	shtDynsym      = 11
	shtProgbitsTls = 1 // .tdata is PROGBITS; SHF_TLS is what marks it thread-local
	shtNote        = 7
	shtInitArray   = 14
	shtFiniArray   = 15
	shtGnuHash     = 0x6ffffff6
	shtGnuVerdef   = 0x6ffffffd
	shtGnuVerneed  = 0x6ffffffe
	shtGnuVersym   = 0x6fffffff

	dtNull        = 0
	dtNeeded      = 1
	dtPltRelSz    = 2
	dtPltGot      = 3
	dtHash        = 4
	dtStrTab      = 5
	dtSymTab      = 6
	dtRela        = 7
	dtRelaSz      = 8
	dtRelaEnt     = 9
	dtStrSz       = 10
	dtSymEnt      = 11
	dtSoname      = 14
	dtRpath       = 15
	dtInit        = 12
	dtFini        = 13
	dtInitArray   = 25
	dtFiniArray   = 26
	dtInitArraySz = 27
	dtFiniArraySz = 28
	dtRunpath     = 29
	dtPltRel      = 20
	dtJmpRel      = 23
	dtFlags       = 30
	dtGnuHash     = 0x6ffffef5
	dtVersym      = 0x6ffffff0
	dtFlags1      = 0x6ffffffb
	dtVerdef      = 0x6ffffffc
	dtVerdefnum   = 0x6ffffffd
	dtVerneed     = 0x6ffffffe
	dtVerneednum  = 0x6fffffff

	// .gnu.version indices: 0 means the symbol is local, 1 the base version (an
	// image's own unversioned global scope); 2 and up name a version definition or
	// requirement. Definitions and requirements share this one numbering, so their
	// indices have to be handed out together.
	verLocal   = 0
	verGlobal  = 1
	verFlgBase = 0x1 // VER_FLG_BASE: the definition standing for the image itself

	dfBindNow = 0x8 // resolve every relocation at load time (no lazy binding)
	df1Now    = 0x1

	// Relocations the dynamic loader applies. JUMP_SLOT binds a PLT's GOT slot to
	// an imported symbol; RELATIVE rebases an absolute address by the load bias.
	R_AARCH64_JUMP_SLOT = 1026
	R_AARCH64_RELATIVE  = 1027
	R_AARCH64_IRELATIVE = 1032
	R_X86_64_JUMP_SLOT  = 7
	R_X86_64_RELATIVE   = 8
	R_X86_64_IRELATIVE  = 37

	sttGnuIfunc = 10 // a symbol whose address a resolver picks at load time

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

	// Require binds an imported symbol to a specific version of the library that
	// defines it, instead of whichever version that library marks as the default.
	// It is how a program pins an older interface a library still carries.
	Require map[string]SymVersion

	// InitArray names functions to run before the image's entry point, and
	// FiniArray functions to run as it is unloaded, each in the order given
	// (DT_INIT_ARRAY / DT_FINI_ARRAY). They are how a library initializes itself
	// and how a language runtime runs its static constructors.
	InitArray []string
	FiniArray []string

	// Runpath is the list of directories the loader searches for this image's
	// libraries, ahead of the system paths (DT_RUNPATH). $ORIGIN in an entry stands
	// for the directory the image itself was loaded from, which is how a program
	// ships libraries beside it without hard-coding an absolute path.
	Runpath []string

	// IFunc maps a symbol to the resolver that decides its address at load time:
	// the loader calls the resolver and uses whatever it returns. It is how one
	// name selects among implementations for the machine it turns out to be
	// running on. Calls to such a symbol must go through the PLT even though it is
	// defined here, since its address is not known until the resolver has run.
	IFunc map[string]string // symbol -> resolver function

	// Lazy resolves each import on its first call rather than at load time,
	// trading a resolver trampoline and a writable GOT for not paying to bind
	// symbols the run never uses. Eager binding is the default and is what
	// hardened builds want (it lets the GOT be made read-only after relocation).
	Lazy bool
}

// SymVersion names a versioned definition of an imported symbol.
type SymVersion struct {
	Library string // the needed library that defines it, e.g. libc.so.6
	Version string // the version to bind to, e.g. GLIBC_2.17
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
		entry:   entrySym,
		interp:  opts.Interp,
		needed:  opts.Needed,
		export:  opts.Export,
		require: opts.Require,
		runpath: opts.Runpath,
		initArr: opts.InitArray,
		finiArr: opts.FiniArray,
		ifunc:   opts.IFunc,
		pie:     opts.PIE,
		lazy:    opts.Lazy,
	})
}

// SharedOptions configures a shared library.
type SharedOptions struct {
	Soname    string   // the name others link against, e.g. libfoo.so.1 (DT_SONAME)
	Needed    []string // shared libraries this one needs (DT_NEEDED)
	Export    []string // symbols to publish for others to resolve against
	Runpath   []string // directories to search for those libraries (DT_RUNPATH)
	InitArray []string // functions run when the library is loaded
	FiniArray []string // functions run when it is unloaded

	// Provide publishes an exported symbol at a named version, so a consumer can
	// bind to that exact interface and the library can carry another alongside it
	// later (DT_VERDEF).
	Provide map[string]string // symbol -> version name

	// IFunc maps a symbol to the resolver that picks its address at load time.
	IFunc map[string]string
}

// WriteSharedLibrary links the object into a shared library: a position-
// independent ET_DYN image with no entry point and no interpreter, publishing
// Export in its dynamic symbol table so the loader (or dlsym) can find them.
func (o *Object) WriteSharedLibrary(opts SharedOptions) ([]byte, error) {
	return o.writeDynImage(dynImage{
		needed:  opts.Needed,
		export:  opts.Export,
		soname:  opts.Soname,
		runpath: opts.Runpath,
		initArr: opts.InitArray,
		finiArr: opts.FiniArray,
		provide: opts.Provide,
		ifunc:   opts.IFunc,
		pie:     true, // a shared library is position-independent by definition
	})
}

// dynImage is the plan for one dynamically linked image.
type dynImage struct {
	entry   string                // entry symbol; empty for a shared library
	interp  string                // loader path; empty for a shared library
	needed  []string              // DT_NEEDED
	export  []string              // symbols published in .dynsym
	require map[string]SymVersion // imports pinned to a specific library version
	provide map[string]string     // exports published at a named version
	runpath []string              // DT_RUNPATH search directories
	initArr []string              // functions run before the entry point
	finiArr []string              // functions run at unload
	ifunc   map[string]string     // symbol -> resolver that picks its address at load
	soname  string                // DT_SONAME; empty for an executable
	pie     bool                  // position-independent (ET_DYN linked at 0)
	lazy    bool                  // resolve imports on first call, not at load
}

// verneed is one library's version requirements.
type verneed struct {
	lib  string
	vers []string
}

// planVersions hands out the .gnu.version index for every version this image
// defines and every version it requires. Definitions and requirements share one
// numbering, so they must be allocated together: 0 and 1 are reserved (local and
// base), definitions take the next indices, and requirements follow.
//
// A definition is keyed by its bare version name; a requirement by library and
// version, since two libraries may use the same version name for different things.
func planVersions(provide map[string]string, require map[string]SymVersion) (defs []string, needs []verneed, index map[string]uint16) {
	index = map[string]uint16{}
	next := uint16(2)

	names := make([]string, 0, len(provide))
	for _, v := range provide {
		names = append(names, v)
	}
	for _, v := range uniqueSorted(names) {
		defs = append(defs, v)
		index[v] = next
		next++
	}

	byLib := map[string][]string{}
	for _, v := range require {
		byLib[v.Library] = append(byLib[v.Library], v.Version)
	}
	libs := make([]string, 0, len(byLib))
	for l := range byLib {
		libs = append(libs, l)
	}
	sort.Strings(libs)
	for _, lib := range libs {
		vers := uniqueSorted(byLib[lib])
		for _, v := range vers {
			index[lib+"\x00"+v] = next
			next++
		}
		needs = append(needs, verneed{lib: lib, vers: vers})
	}
	return defs, needs, index
}

// buildVerdef encodes .gnu.version_d: the base definition standing for the image
// itself, then one definition per version it publishes. As with .gnu.version_r the
// offsets are relative to the record they sit in, and a zero next-offset ends the
// list.
func buildVerdef(base string, defs []string, index map[string]uint16, dynstr *strtab) []byte {
	const vdSz, vdaSz = 20, 8
	b := &elfBuf{}
	entry := func(flags, ndx uint16, name string, last bool) {
		next := uint32(vdSz + vdaSz)
		if last {
			next = 0
		}
		b.u16(1)     // vd_version
		b.u16(flags) // vd_flags
		b.u16(ndx)   // vd_ndx
		b.u16(1)     // vd_cnt: one name per definition
		b.u32(elfHash(name))
		b.u32(vdSz) // vd_aux: the Verdaux follows immediately
		b.u32(next)
		b.u32(dynstr.add(name)) // vda_name
		b.u32(0)                // vda_next: the only one
	}
	entry(verFlgBase, verGlobal, base, len(defs) == 0)
	for i, v := range defs {
		entry(0, index[v], v, i == len(defs)-1)
	}
	return b.b
}

// uniqueSorted returns the sorted distinct elements of in.
func uniqueSorted(in []string) []string {
	sort.Strings(in)
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
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
	// A symbol named as an ifunc has no body of its own -- its address is whatever
	// its resolver returns -- so it arrives here looking undefined. Pull those out
	// of the imports: they are not resolved in a library but by calling our own
	// resolver at load time. Both kinds still need a PLT slot, since neither
	// address is known at link time.
	// A thread-local reached through the GOT is a variable, not a function: it gets
	// a GOT slot but never a PLT stub, so keep it out of the imports.
	tlsGotSym := map[string]bool{}
	for _, r := range o.allRelocs() {
		if isTLSGotReloc(r.Type) || isTLSGdReloc(r.Type) {
			tlsGotSym[r.Sym] = true
		}
	}
	var imports, ifuncs []string
	for _, n := range o.imports() {
		switch {
		case tlsGotSym[n]:
			// resolved through its GOT slot below, not through a PLT
		case im.ifunc[n] != "":
			ifuncs = append(ifuncs, n)
		default:
			imports = append(imports, n)
		}
	}
	for _, n := range ifuncs {
		if r := im.ifunc[n]; !o.definesSym(r) {
			return nil, fmt.Errorf("obj: ifunc resolver %q for %q is not defined here", r, n)
		}
	}
	pltNames := append(append([]string(nil), imports...), ifuncs...)
	nplt := len(pltNames)

	// Initial-exec reads a thread-local's offset from a GOT slot the loader fills.
	// Collect the variables reached that way; each needs a slot and a relocation
	// telling the loader which variable it describes.
	var tlsGot, tlsGd []string
	seenTls, seenGd := map[string]bool{}, map[string]bool{}
	for _, r := range o.allRelocs() {
		switch {
		case isTLSGotReloc(r.Type) && !seenTls[r.Sym]:
			seenTls[r.Sym] = true
			tlsGot = append(tlsGot, r.Sym)
		case isTLSGdReloc(r.Type) && !seenGd[r.Sym]:
			seenGd[r.Sym] = true
			tlsGd = append(tlsGd, r.Sym)
		}
	}
	sort.Strings(tlsGot)
	sort.Strings(tlsGd)
	var tlsHere, tlsElsewhere []string
	for _, n := range append(append([]string(nil), tlsGot...), tlsGd...) {
		if o.definesSym(n) {
			tlsHere = append(tlsHere, n)
		} else {
			tlsElsewhere = append(tlsElsewhere, n)
		}
	}

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
		if nplt > 0 || len(tlsGot) > 0 || len(tlsGd) > 0 {
			nRel++ // GOT[0] holds &_DYNAMIC, itself an absolute address
		}
		// Each init/fini entry is a function address, so it needs rebasing too.
		nRel += len(im.initArr) + len(im.finiArr)
	}
	// The thread-local GOT slots need a relocation each, in every image: their
	// values are the loader's to decide, position-independent or not.
	var tlsSlotRel []Reloc
	nRelaDyn := nRel + len(tlsGot) + 2*len(tlsGd)

	// --- section layout; vaddr = base + file offset throughout ---------------
	// PT_PHDR is not decoration: the loader derives the image's load bias from it
	// (l_addr = where it actually mapped the headers - this p_vaddr). Without it
	// the bias reads as zero, which is right for a fixed-base image but sends the
	// loader dereferencing garbage in a PIE.
	nph := 6 // PT_PHDR, PT_NOTE, PT_LOAD (r-x), PT_LOAD (rw-), PT_DYNAMIC, PT_GNU_STACK
	var interp []byte
	if im.interp != "" {
		interp = append([]byte(im.interp), 0)
		nph++ // PT_INTERP
	}
	// Eager binding writes every GOT slot before any of the image's code runs, so
	// the GOT and .dynamic can be frozen once relocation is done -- the point of
	// binding eagerly. Lazy binding needs the GOT writable for its resolver, so it
	// gets no RELRO.
	relro := !im.lazy
	if relro {
		nph++ // PT_GNU_RELRO
	}
	if len(o.Tdata) > 0 {
		nph++ // PT_TLS
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
	// DT_GNU_HASH walks a bucket as a run of adjacent symbols, so the exports must
	// be ordered by bucket. That count depends only on how many there are, so the
	// order can be settled before anything is laid out.
	undef := append(append([]string(nil), imports...), tlsElsewhere...)
	defined := append(append([]string(nil), exports...), tlsHere...)
	symoffset := 1 + len(undef)
	nbuckets := uint32(1)
	if len(defined) > 0 {
		nbuckets = uint32(len(defined))
	}
	sort.SliceStable(defined, func(i, j int) bool {
		return gnuHash(defined[i])%nbuckets < gnuHash(defined[j])%nbuckets
	})

	dynNames := append([]string{""}, undef...)
	dynNames = append(dynNames, defined...)
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
	var runpathOff uint32
	if len(im.runpath) > 0 {
		runpathOff = dynstr.add(strings.Join(im.runpath, ":"))
	}

	// Versions: .gnu.version gives every dynamic symbol an index, .gnu.version_d
	// spells out the versions this image defines, and .gnu.version_r the ones it
	// needs from its libraries.
	defs, needs, verIndex := planVersions(im.provide, im.require)
	versioned := len(defs) > 0 || len(needs) > 0
	var versym, verdefTab, verneedTab []byte
	if versioned {
		for _, n := range needs {
			dynstr.add(n.lib)
			for _, v := range n.vers {
				dynstr.add(v)
			}
		}
		idx := make([]uint16, nsym)
		for i := 1; i < nsym; i++ {
			idx[i] = verGlobal
		}
		for i, n := range dynNames {
			if i == 0 {
				continue
			}
			if v, ok := im.require[n]; ok {
				if vi, ok := verIndex[v.Library+"\x00"+v.Version]; ok {
					idx[i] = vi
				}
			}
			if v, ok := im.provide[n]; ok {
				if vi, ok := verIndex[v]; ok {
					idx[i] = vi
				}
			}
		}
		idx[0] = verLocal
		vb := &elfBuf{}
		for _, v := range idx {
			vb.u16(v)
		}
		versym = vb.b
		if len(defs) > 0 {
			base := im.soname
			if base == "" {
				base = "cg12" // an executable has no soname to name its base version after
			}
			verdefTab = buildVerdef(base, defs, verIndex, dynstr)
		}
		if len(needs) > 0 {
			verneedTab = buildVerneed(needs, verIndex, dynstr)
		}
	}

	hash := sysvHash(dynNames)
	gnuHashTab := gnuHashTable(dynNames, symoffset)

	off := alignUp(64+nph*56, 8)
	interpOff := off
	off += len(interp)
	off = alignUp(off, 4)
	note, noteDescOff := buildIDNote()
	noteOff := off
	off += len(note)
	off = alignUp(off, 8)
	hashOff := off
	off += len(hash)
	off = alignUp(off, 8)
	gnuHashOff := off
	off += len(gnuHashTab)
	dynsymOff := off
	off += nsym * 24
	dynstrOff := off
	off += len(dynstr.b)
	off = alignUp(off, 2)
	gnuVersymOff := off
	off += len(versym)
	off = alignUp(off, 4)
	gnuVerdefOff := off
	off += len(verdefTab)
	gnuVerneedOff := off
	off += len(verneedTab)
	off = alignUp(off, 8)
	relaDynOff := off
	off += nRelaDyn * 24
	relaPltOff := off
	off += nplt * 24
	off = alignUp(off, 16)
	plt0Sz := plt0Size(o.Machine, im.lazy && nplt > 0)
	plt0Off := off
	pltOff := off + plt0Sz
	off = pltOff + nplt*stubSz
	off = alignUp(off, max(16, alignOr(o.TextAlign, 4)))
	textOff := off
	off += len(o.Text)
	// .rodata belongs in the read-only region, which is what makes it read-only:
	// the promise is kept by the mapping, not by the source's types.
	rodataOff := alignUp(off, alignOr(o.RodataAlign, 8))
	off = rodataOff + len(o.Rodata)
	roEnd := off

	// How much of the writable region is read-only after relocation: the GOT and
	// .dynamic. Both sizes are known before placement, which is what lets the
	// region be positioned to end exactly on a page (see below).
	// The GOT holds the psABI's reserved header, then one slot per PLT entry, then
	// one per thread-local reached through it. The header is there whenever the GOT
	// is, so its size and its contents below must agree on that -- they are written
	// in different places, and a disagreement silently shifts everything after it.
	gotSize, tlsGotAt, tlsGdAt := 0, 0, 0
	if nplt > 0 || len(tlsGot) > 0 || len(tlsGd) > 0 {
		gotSize = (gotReserved + nplt) * 8
		tlsGotAt = gotSize
		gotSize += len(tlsGot) * 8
		// A general-dynamic descriptor is two words: the owning module, then the
		// offset within it.
		tlsGdAt = gotSize
		gotSize += len(tlsGd) * 16
	}
	// The init/fini arrays are function-pointer tables: written by relocation and
	// never again, so they belong in the relro region beside the GOT.
	initArrSize, finiArrSize := len(im.initArr)*8, len(im.finiArr)*8

	// .data.rel.ro is const data holding an address: the same story as the GOT,
	// arrived at from the other side. It is written by relocation and never again,
	// so it belongs in the relro region too.
	relroDataAlign := alignOr(o.RelroAlign, 8)

	// .tdata is only ever read (each thread's block is a copy of it), so it too
	// sits in the relro region -- at its head, where the alignment below can be
	// arranged for it.
	tlsAlign := o.TlsAlign
	if tlsAlign < 1 {
		tlsAlign = 1
	}
	tdataSize := len(o.Tdata)
	ndyn := len(im.needed) + 7 // NEEDED..., HASH, GNU_HASH, STRTAB, SYMTAB, STRSZ, SYMENT, NULL
	if !im.lazy {
		ndyn += 2 // FLAGS, FLAGS_1 (eager binding)
	}
	if im.soname != "" {
		ndyn++ // SONAME
	}
	if len(im.runpath) > 0 {
		ndyn++ // RUNPATH
	}
	if nplt > 0 {
		ndyn += 4 // PLTGOT, PLTRELSZ, PLTREL, JMPREL
	}
	if nRelaDyn > 0 {
		ndyn += 3 // RELA, RELASZ, RELAENT
	}
	if versioned {
		ndyn++ // VERSYM
	}
	if len(im.provide) > 0 {
		ndyn += 2 // VERDEF, VERDEFNUM
	}
	if len(im.require) > 0 {
		ndyn += 2 // VERNEED, VERNEEDNUM
	}
	if initArrSize > 0 {
		ndyn += 2 // INIT_ARRAY, INIT_ARRAYSZ
	}
	if finiArrSize > 0 {
		ndyn += 2 // FINI_ARRAY, FINI_ARRAYSZ
	}
	// .data.rel.ro sits after .tdata and before the GOT: its offset within the
	// region reserves whatever padding its own alignment needs, and its size is
	// rounded to 8 because everything after it here is a table of words and is
	// placed by addition alone.
	relroDataOffInRegion := alignUp(tdataSize, relroDataAlign)
	relroDataSize := alignUp(len(o.Relro), 8)
	relroSize := relroDataOffInRegion + relroDataSize + initArrSize + finiArrSize + gotSize + ndyn*16
	// PT_TLS must start on its own alignment, and so must .data.rel.ro. The
	// writable region is placed by working back from its end (below), so rounding
	// the region's size up makes its *start* land aligned -- and with it .tdata, at
	// the head, and .data.rel.ro, at a rounded offset from the head. Both must be
	// satisfied, and alignments are powers of two, so the larger implies the other.
	relroSize = alignUp(relroSize, max(tlsAlign, relroDataAlign))

	// The writable region begins past the read-execute one's last page, so the two
	// never share a page. When there is a relro region it is nudged forward within
	// that page so that it *ends* on a page boundary: .data then starts on a fresh
	// page (the loader freezes whole pages, and .data must stay writable) without
	// costing a page of padding to get there. The segment start need not be page
	// aligned -- p_vaddr and p_offset stay congruent because they differ by the
	// image base throughout.
	rwOff := alignUp(roEnd, execAlign)
	if relro {
		rwOff += (execAlign - relroSize%execAlign) % execAlign
	}
	tdataOff := rwOff
	relroDataOff := rwOff + relroDataOffInRegion
	initArrOff := relroDataOff + relroDataSize
	finiArrOff := initArrOff + initArrSize
	gotOff := finiArrOff + finiArrSize
	dynamicOff := gotOff + gotSize // every size here is a multiple of 8, so this stays aligned
	off = dynamicOff + ndyn*16

	// .data starts where its own contents need it to, not merely 8-aligned: a
	// datum padded to 16 within the section is only 16-aligned if the section is.
	dataOff := alignUp(off, alignOr(o.DataAlign, 8))
	relroEnd := off // == rwOff + relroSize, a page boundary when relro is on
	if relro {
		dataOff = relroEnd // a page boundary, which satisfies any datum's alignment
	}
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
	secNote := nextSec()
	secHash := nextSec()
	secGnuHash := nextSec()
	secDynsym := nextSec()
	secDynstr := nextSec()
	secVersym, secVerdef, secVerneed := -1, -1, -1
	if versioned {
		secVersym = nextSec()
	}
	if len(verdefTab) > 0 {
		secVerdef = nextSec()
	}
	if len(verneedTab) > 0 {
		secVerneed = nextSec()
	}
	secRelaDyn := -1
	if nRelaDyn > 0 {
		secRelaDyn = nextSec()
	}
	secRelaPlt, secPlt, secGot := -1, -1, -1
	if nplt > 0 {
		secRelaPlt = nextSec()
		secPlt = nextSec()
	}
	secText := nextSec()
	secTdata, secTbss := -1, -1
	if tdataSize > 0 {
		secTdata = nextSec()
	}
	if o.TbssSize > 0 {
		secTbss = nextSec()
	}
	secInitArr, secFiniArr := -1, -1
	if initArrSize > 0 {
		secInitArr = nextSec()
	}
	if finiArrSize > 0 {
		secFiniArr = nextSec()
	}
	if nplt > 0 || len(tlsGot) > 0 || len(tlsGd) > 0 {
		secGot = nextSec()
	}
	secDynamic := nextSec()
	secData := -1
	if len(o.Data) > 0 {
		secData = nextSec()
	}
	secBss := -1
	if o.BssSize > 0 {
		secBss = nextSec()
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
		case SecRodata:
			symVaddr[s.Name] = va(rodataOff) + s.Value
		case SecRelro:
			symVaddr[s.Name] = va(relroDataOff) + s.Value
		case SecBss:
			// .bss has no bytes in the file; it picks up where .data ends in memory,
			// rounded up to what its own contents need. Without the rounding it
			// begins wherever .data happened to stop -- so a 5-byte string in .data
			// leaves every .bss word on an odd address, which arm64 tolerates for
			// ordinary loads and faults on for an exclusive.
			symVaddr[s.Name] = va(dataOff) + uint64(bssStart(o)) + s.Value
		}
	}
	for i, n := range pltNames {
		symVaddr[n] = va(pltOff + i*stubSz)
	}
	// A thread-local has no address: every thread has its own copy. Its symbol
	// value is an offset within the TLS block, which a local-exec relocation turns
	// into an offset from the thread pointer.
	tlsOff := map[string]uint64{}
	blockSize := uint64(tdataSize + o.TbssSize)
	for _, sym := range o.Syms {
		switch sym.Section {
		case SecTdata:
			tlsOff[sym.Name] = tpOffset(o.Machine, sym.Value, blockSize, uint64(tlsAlign))
		case SecTbss:
			// .tbss carries no image, so it follows .tdata within each thread's block.
			tlsOff[sym.Name] = tpOffset(o.Machine, uint64(tdataSize)+sym.Value, blockSize, uint64(tlsAlign))
		}
	}
	var entry uint64
	if im.entry != "" {
		e, ok := symVaddr[im.entry]
		if !ok {
			return nil, fmt.Errorf("obj: entry symbol %q is not defined", im.entry)
		}
		entry = e
	}

	// Each thread-local GOT slot is filled by the loader with that variable's
	// offset from the thread pointer -- which only the loader can know, since it
	// decides where each module's block goes.
	tlsGotVaddr := map[string]uint64{}
	for i, n := range tlsGot {
		tlsGotVaddr[n] = va(gotOff + tlsGotAt + i*8)
	}
	tlsGdVaddr := map[string]uint64{}
	for i, n := range tlsGd {
		tlsGdVaddr[n] = va(gotOff + tlsGdAt + i*16)
	}

	text := append([]byte(nil), o.Text...)
	data := append([]byte(nil), o.Data...)
	rodata := append([]byte(nil), o.Rodata...)
	dynRel, err := resolveSection(o.Machine, im.pie, text, va(textOff), o.Relocs, symVaddr, tlsOff, tlsGotVaddr, tlsGdVaddr)
	if err != nil {
		return nil, err
	}
	dataRel, err := resolveSection(o.Machine, im.pie, data, va(dataOff), o.DataRelocs, symVaddr, tlsOff, tlsGotVaddr, tlsGdVaddr)
	if err != nil {
		return nil, err
	}
	dynRel = append(dynRel, dataRel...)

	// .rodata is mapped without write permission, so anything the loader would
	// have to patch there cannot be. In a fixed-base image every address is a
	// link-time constant and resolveSection writes them all now, leaving nothing
	// for the loader; in a PIE they become RELATIVE relocations, and there is
	// nowhere to apply them. A real toolchain answers this with .data.rel.ro --
	// mapped writable, relocated, then made read-only by PT_GNU_RELRO -- which
	// cg12 has for the GOT and .dynamic but not yet for data.
	rodataRel, err := resolveSection(o.Machine, im.pie, rodata, va(rodataOff), o.RodataRelocs, symVaddr, tlsOff, tlsGotVaddr, tlsGdVaddr)
	if err != nil {
		return nil, err
	}
	relroData := append([]byte(nil), o.Relro...)
	relroRel, err := resolveSection(o.Machine, im.pie, relroData, va(relroDataOff), o.RelroRelocs, symVaddr, tlsOff, tlsGotVaddr, tlsGdVaddr)
	if err != nil {
		return nil, err
	}
	// Whatever is left for the loader here is exactly what the section is for, so
	// unlike .rodata's there is nothing to refuse.
	dynRel = append(dynRel, relroRel...)
	if len(rodataRel) > 0 {
		return nil, fmt.Errorf("obj: %d relocation(s) in .rodata would have to be applied by the loader, "+
			"which cannot write it: read-only data holding an address needs .data.rel.ro, which this linker does not emit yet",
			len(rodataRel))
	}
	if im.pie && (nplt > 0 || len(tlsGot) > 0 || len(tlsGd) > 0) {
		dynRel = append(dynRel, Reloc{
			Offset: va(gotOff), Type: relativeType(o.Machine), Addend: int64(va(dynamicOff)),
		})
	}

	// The init/fini tables hold the addresses of functions defined here.
	funcArray := func(names []string, at int) (*elfBuf, error) {
		b := &elfBuf{}
		for i, n := range names {
			if !o.definesSym(n) {
				return nil, fmt.Errorf("obj: init/fini function %q is not defined here", n)
			}
			addr := symVaddr[n]
			b.u64(addr)
			if im.pie {
				dynRel = append(dynRel, Reloc{
					Offset: va(at + i*8), Type: relativeType(o.Machine), Addend: int64(addr),
				})
			}
		}
		return b, nil
	}
	initArr, err := funcArray(im.initArr, initArrOff)
	if err != nil {
		return nil, err
	}
	finiArr, err := funcArray(im.finiArr, finiArrOff)
	if err != nil {
		return nil, err
	}
	if len(dynRel) != nRel {
		return nil, fmt.Errorf("obj: counted %d relative relocations, produced %d", nRel, len(dynRel))
	}
	// Tell the loader which variable each thread-local GOT slot describes; it fills
	// in the offset once it has placed that module's block.
	for _, n := range tlsGot {
		tlsSlotRel = append(tlsSlotRel, Reloc{
			Offset: tlsGotVaddr[n], Sym: n, Type: tlsGotSlotType(o.Machine),
		})
	}
	// A descriptor's two words are filled separately: which module owns the
	// variable, and its offset within that module.
	for _, n := range tlsGd {
		tlsSlotRel = append(tlsSlotRel,
			Reloc{Offset: tlsGdVaddr[n], Sym: n, Type: R_AARCH64_TLS_DTPMOD64},
			Reloc{Offset: tlsGdVaddr[n] + 8, Sym: n, Type: R_AARCH64_TLS_DTPREL64},
		)
	}

	// --- section contents ----------------------------------------------------
	dynsym := &elfBuf{}
	dynsym.pad(24) // index 0: the null symbol
	dynIndex := map[string]int{}
	for i, n := range dynNames[1:] {
		idx := i + 1
		dynIndex[n] = idx
		if idx < symoffset {
			// Undefined here: the loader finds it in a needed library. A thread-local
			// is still STT_TLS, which is how the loader knows to resolve it against a
			// module's block rather than as an address.
			typ := byte(sttFunc)
			if tlsGotSym[n] {
				typ = sttTLS
			}
			dynsym.u32(symNameOff[idx])
			dynsym.u8((stbGlobal << 4) | typ)
			dynsym.u8(0)
			dynsym.u16(shnUndef)
			dynsym.u64(0) // st_value
			dynsym.u64(0) // st_size
			continue
		}
		// Defined here: publish its value, size, and section. A thread-local's value
		// is its offset within this module's block, not an address.
		sym := o.findSym(n)
		typ, shndx, value := byte(sttObject), secData, symVaddr[n]
		switch {
		case sym.Section == SecTdata:
			typ, shndx, value = sttTLS, secTdata, sym.Value
		case sym.Section == SecTbss:
			typ, shndx, value = sttTLS, secTbss, uint64(tdataSize)+sym.Value
		case sym.Section == SecText:
			shndx = secText
			if sym.Func {
				typ = sttFunc
			}
		case sym.Section == SecBss:
			shndx = secBss
		case sym.Func:
			typ = sttFunc
		}
		dynsym.u32(symNameOff[idx])
		dynsym.u8((stbGlobal << 4) | typ)
		dynsym.u8(0)
		dynsym.u16(uint16(shndx))
		dynsym.u64(value)
		dynsym.u64(sym.Size)
	}

	relaDyn := &elfBuf{}
	for _, r := range dynRel {
		relaDyn.u64(r.Offset)
		relaDyn.u64(uint64(r.Type)) // symbol index 0: RELATIVE needs no symbol
		relaDyn.i64(r.Addend)
	}
	for _, r := range tlsSlotRel {
		relaDyn.u64(r.Offset)
		relaDyn.u64(uint64(dynIndex[r.Sym])<<32 | uint64(r.Type)) // names the variable
		relaDyn.i64(r.Addend)
	}

	relaPlt := &elfBuf{}
	for i, n := range pltNames {
		relaPlt.u64(va(gotOff + (gotReserved+i)*8)) // r_offset: the GOT slot
		if r, ok := im.ifunc[n]; ok {
			// The loader calls the resolver and stores what it returns. No symbol is
			// named: the addend *is* the resolver, which the loader rebases first.
			relaPlt.u64(uint64(irelativeType(o.Machine)))
			relaPlt.i64(int64(symVaddr[r]))
			continue
		}
		relaPlt.u64(uint64(i+1)<<32 | uint64(jumpSlot(o.Machine))) // dynsym index i+1
		relaPlt.i64(0)
	}

	plt := &elfBuf{}
	if plt0Sz > 0 {
		plt.bytes(plt0Stub(o.Machine, va(plt0Off), va(gotOff)))
	}
	for i := range pltNames {
		plt.bytes(pltStub(o.Machine, va(pltOff+i*stubSz), va(gotOff+(gotReserved+i)*8), va(plt0Off), i, im.lazy))
	}

	got := &elfBuf{}
	if nplt > 0 || len(tlsGot) > 0 || len(tlsGd) > 0 {
		got.u64(va(dynamicOff)) // GOT[0] = &_DYNAMIC
		got.u64(0)              // GOT[1], GOT[2]: the loader's lazy-resolution slots
		got.u64(0)
		for i, n := range pltNames {
			// The loader fills this from the slot's relocation; under lazy binding it
			// adds the load bias to this initial value instead. An ifunc is always
			// resolved eagerly, so its slot needs no lazy trampoline value.
			_, isIfunc := im.ifunc[n]
			got.u64(pltGotInit(o.Machine, im.lazy && !isIfunc, va(plt0Off), va(pltOff+i*stubSz)))
		}
		for range tlsGot {
			got.u64(0) // the loader writes the thread-pointer offset here
		}
		for range tlsGd {
			got.u64(0) // the loader writes the owning module here...
			got.u64(0) // ...and the offset within it here
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
	if len(im.runpath) > 0 {
		dt(dtRunpath, uint64(runpathOff))
	}
	dt(dtHash, va(hashOff))
	dt(dtGnuHash, va(gnuHashOff))
	dt(dtStrTab, va(dynstrOff))
	dt(dtSymTab, va(dynsymOff))
	dt(dtStrSz, uint64(len(dynstr.b)))
	dt(dtSymEnt, 24)
	if nRelaDyn > 0 {
		dt(dtRela, va(relaDynOff))
		dt(dtRelaSz, uint64(len(relaDyn.b)))
		dt(dtRelaEnt, 24)
	}
	if initArrSize > 0 {
		dt(dtInitArray, va(initArrOff))
		dt(dtInitArraySz, uint64(initArrSize))
	}
	if finiArrSize > 0 {
		dt(dtFiniArray, va(finiArrOff))
		dt(dtFiniArraySz, uint64(finiArrSize))
	}
	if nplt > 0 {
		dt(dtPltGot, va(gotOff))
		dt(dtPltRelSz, uint64(len(relaPlt.b)))
		dt(dtPltRel, dtRela)
		dt(dtJmpRel, va(relaPltOff))
	}
	if versioned {
		dt(dtVersym, va(gnuVersymOff))
	}
	if len(verdefTab) > 0 {
		dt(dtVerdef, va(gnuVerdefOff))
		dt(dtVerdefnum, uint64(len(defs)+1)) // the definitions plus the base
	}
	if len(verneedTab) > 0 {
		dt(dtVerneed, va(gnuVerneedOff))
		dt(dtVerneednum, uint64(len(needs)))
	}
	if !im.lazy {
		dt(dtFlags, dfBindNow)
		dt(dtFlags1, df1Now)
	}
	dt(dtNull, 0)
	if n := len(dyn.b) / 16; n != ndyn {
		return nil, fmt.Errorf("obj: dynamic section has %d entries, reserved %d", n, ndyn)
	}

	// --- emit ----------------------------------------------------------------
	out := &elfBuf{}
	out.pad(64) // ELF header, filled in at the end
	phdrMem := func(typ, flags uint32, off, vaddr, filesz, memsz, align uint64) {
		out.u32(typ)
		out.u32(flags)
		out.u64(off)
		out.u64(vaddr)
		out.u64(vaddr) // p_paddr
		out.u64(filesz)
		out.u64(memsz)
		out.u64(align)
	}
	phdr := func(typ, flags uint32, off, vaddr, size, align uint64) {
		phdrMem(typ, flags, off, vaddr, size, size, align)
	}
	// PT_PHDR must precede every loadable segment, and PT_INTERP conventionally
	// follows it. Both live inside the read-execute PT_LOAD that starts the image.
	phdr(ptPhdr, pfR, 64, va(64), uint64(nph*56), 8)
	if interp != nil {
		phdr(ptInterp, pfR, uint64(interpOff), va(interpOff), uint64(len(interp)), 1)
	}
	phdr(ptNote, pfR, uint64(noteOff), va(noteOff), uint64(len(note)), 4)
	phdr(ptLoad, pfR|pfX, 0, base, uint64(roEnd), execAlign)
	// .bss lives past the file's end: memsz exceeds filesz and the loader zeroes
	// the difference.
	phdrMem(ptLoad, pfR|pfW, uint64(rwOff), va(rwOff), uint64(rwEnd-rwOff),
		uint64(dataOff+bssStart(o)+o.BssSize-rwOff), execAlign)
	phdr(ptDynamic, pfR|pfW, uint64(dynamicOff), va(dynamicOff), uint64(ndyn*16), 8)
	if relro {
		// Read-only after relocation: the GOT and .dynamic, up to the page boundary
		// that .data starts on. Stays inside the writable segment, which is what the
		// loader is allowed to re-protect.
		phdr(ptGnuRelro, pfR, uint64(rwOff), va(rwOff), uint64(relroEnd-rwOff), 1)
	}
	if tdataSize > 0 {
		// The template every thread's TLS block is built from. It is never written,
		// only copied, so it is read-only.
		phdrMem(ptTls, pfR, uint64(tdataOff), va(tdataOff), uint64(tdataSize),
			uint64(tdataSize+o.TbssSize), uint64(tlsAlign))
	}
	// An empty segment whose flags say the stack wants no execute permission. Its
	// absence is not neutral: the kernel then falls back to an executable stack.
	phdr(ptGnuStack, pfR|pfW, 0, 0, 0, 0x10)

	put := func(fileOff int, b []byte) {
		for len(out.b) < fileOff {
			out.u8(0)
		}
		out.bytes(b)
	}
	if interp != nil {
		put(interpOff, interp)
	}
	put(noteOff, note)
	put(hashOff, hash)
	put(gnuHashOff, gnuHashTab)
	put(dynsymOff, dynsym.b)
	put(dynstrOff, dynstr.b)
	if versioned {
		put(gnuVersymOff, versym)
		put(gnuVerdefOff, verdefTab)
		put(gnuVerneedOff, verneedTab)
	}
	put(relaDynOff, relaDyn.b)
	put(relaPltOff, relaPlt.b)
	put(plt0Off, plt.b)
	put(textOff, text)
	put(rodataOff, rodata)
	put(tdataOff, o.Tdata)
	put(relroDataOff, relroData)
	put(initArrOff, initArr.b)
	put(finiArrOff, finiArr.b)
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
	set(secNote, ".note.gnu.build-id", shtNote, shfAlloc, noteOff, len(note), 0, 0, 4, 0)
	set(secHash, ".hash", shtHash, shfAlloc, hashOff, len(hash), secDynsym, 0, 8, 4)
	set(secGnuHash, ".gnu.hash", shtGnuHash, shfAlloc, gnuHashOff, len(gnuHashTab), secDynsym, 0, 8, 0)
	set(secDynsym, ".dynsym", shtDynsym, shfAlloc, dynsymOff, len(dynsym.b), secDynstr, 1, 8, 24)
	set(secDynstr, ".dynstr", shtStrtab, shfAlloc, dynstrOff, len(dynstr.b), 0, 0, 1, 0)
	set(secVersym, ".gnu.version", shtGnuVersym, shfAlloc, gnuVersymOff, len(versym), secDynsym, 0, 2, 2)
	set(secVerdef, ".gnu.version_d", shtGnuVerdef, shfAlloc, gnuVerdefOff, len(verdefTab), secDynstr, len(defs)+1, 4, 0)
	set(secVerneed, ".gnu.version_r", shtGnuVerneed, shfAlloc, gnuVerneedOff, len(verneedTab), secDynstr, len(needs), 4, 0)
	set(secRelaDyn, ".rela.dyn", shtRela, shfAlloc, relaDynOff, len(relaDyn.b), secDynsym, 0, 8, 24)
	set(secRelaPlt, ".rela.plt", shtRela, shfAlloc, relaPltOff, len(relaPlt.b), secDynsym, secPlt, 8, 24)
	set(secPlt, ".plt", shtProgbits, shfAlloc|shfExecinstr, plt0Off, len(plt.b), 0, 0, 16, 0)
	set(secText, ".text", shtProgbits, shfAlloc|shfExecinstr, textOff, len(text), 0, 0, 16, 0)
	set(secTdata, ".tdata", shtProgbitsTls, shfAlloc|shfWrite|shfTLS, tdataOff, tdataSize, 0, 0, uint64(tlsAlign), 0)
	set(secInitArr, ".init_array", shtInitArray, shfAlloc|shfWrite, initArrOff, initArrSize, 0, 0, 8, 8)
	set(secFiniArr, ".fini_array", shtFiniArray, shfAlloc|shfWrite, finiArrOff, finiArrSize, 0, 0, 8, 8)
	set(secGot, ".got.plt", shtProgbits, shfAlloc|shfWrite, gotOff, len(got.b), 0, 0, 8, 8)
	set(secDynamic, ".dynamic", shtDynamic, shfAlloc|shfWrite, dynamicOff, ndyn*16, secDynstr, 0, 8, 16)
	set(secData, ".data", shtProgbits, shfAlloc|shfWrite, dataOff, len(data), 0, 0, 8, 0)
	// NOBITS: these take up addresses and run-time space but no file bytes, so
	// their offsets are only where they would have begun.
	set(secBss, ".bss", shtNobits, shfAlloc|shfWrite, dataOff+bssStart(o), o.BssSize, 0, 0, uint64(alignOr(o.BssAlign, 8)), 0)
	set(secTbss, ".tbss", shtNobits, shfAlloc|shfWrite|shfTLS, tdataOff+tdataSize, o.TbssSize, 0, 0, uint64(tlsAlign), 0)

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
	setBuildID(out.b, noteOff+noteDescOff)
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

// allRelocs returns every relocation the object carries against its sections.
func (o *Object) allRelocs() []Reloc {
	all := append([]Reloc{}, o.Relocs...)
	all = append(all, o.DataRelocs...)
	all = append(all, o.RodataRelocs...)
	return append(all, o.RelroRelocs...)
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
func resolveSection(machine uint16, pie bool, sec []byte, secVaddr uint64, relocs []Reloc, symVaddr, tlsOff, gotVaddr, gdVaddr map[string]uint64) ([]Reloc, error) {
	var dyn []Reloc
	for _, r := range relocs {
		if isTLSGdReloc(r.Type) {
			// The code addresses the descriptor; the loader fills it in. So this
			// resolves like any other reference to an address -- the descriptor's.
			d, ok := gdVaddr[r.Sym]
			if !ok {
				return nil, fmt.Errorf("obj: no descriptor for thread-local %q", r.Sym)
			}
			place := int64(secVaddr) + int64(r.Offset)
			var err error
			switch r.Type {
			case R_AARCH64_TLSGD_ADR_PAGE21:
				err = resolveAArch64(sec, Reloc{Offset: r.Offset, Sym: r.Sym,
					Type: R_AARCH64_ADR_PREL_PG_HI21}, int64(d), place)
			case R_AARCH64_TLSGD_ADD_LO12_NC:
				err = resolveAArch64(sec, Reloc{Offset: r.Offset, Sym: r.Sym,
					Type: R_AARCH64_ADD_ABS_LO12_NC}, int64(d), place)
			}
			if err != nil {
				return nil, err
			}
			continue
		}
		if isTLSGotReloc(r.Type) {
			// Initial-exec: the code addresses the GOT slot, and the loader puts the
			// thread-pointer offset in it. So this resolves like any other reference
			// to an address -- the slot's.
			slot, ok := gotVaddr[r.Sym]
			if !ok {
				return nil, fmt.Errorf("obj: no GOT slot for thread-local %q", r.Sym)
			}
			if err := resolveTLSGot(machine, sec, r, int64(slot), int64(secVaddr)+int64(r.Offset)); err != nil {
				return nil, err
			}
			continue
		}
		if isTLSReloc(r.Type) || isX86TLSReloc(r.Type) {
			off, ok := tlsOff[r.Sym]
			if !ok {
				return nil, fmt.Errorf("obj: thread-local symbol %q is not defined here; only local-exec TLS is supported, which needs the variable in this image", r.Sym)
			}
			if err := resolveTLS(sec, r, int64(off)+r.Addend); err != nil {
				return nil, err
			}
			continue
		}
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

// resolveTLSGot points an initial-exec reference at the GOT slot holding the
// variable's thread-pointer offset. The addressing is the ordinary PC-relative
// kind: what makes it thread-local is only what the loader stores in the slot.
func resolveTLSGot(machine uint16, sec []byte, r Reloc, slot, place int64) error {
	switch r.Type {
	case R_AARCH64_TLSIE_ADR_GOTTPREL_PAGE21:
		page := ((slot &^ 0xfff) - (place &^ 0xfff)) >> 12
		if page < -(1<<20) || page >= (1<<20) {
			return fmt.Errorf("obj: thread-local GOT slot for %q out of adrp range", r.Sym)
		}
		w := binary.LittleEndian.Uint32(sec[r.Offset:])
		w = (w &^ ((3 << 29) | (0x7ffff << 5))) | (uint32(page&3) << 29) | (uint32((page>>2)&0x7ffff) << 5)
		binary.LittleEndian.PutUint32(sec[r.Offset:], w)
	case R_AARCH64_TLSIE_LD64_GOTTPREL_LO12_NC:
		w := binary.LittleEndian.Uint32(sec[r.Offset:])
		w = (w &^ (0xfff << 10)) | (uint32((slot&0xfff)/8) << 10) // ldr scales by 8
		binary.LittleEndian.PutUint32(sec[r.Offset:], w)
	case R_X86_64_GOTTPOFF:
		disp := slot + r.Addend - place
		if disp < math.MinInt32 || disp > math.MaxInt32 {
			return fmt.Errorf("obj: thread-local GOT slot for %q out of range", r.Sym)
		}
		binary.LittleEndian.PutUint32(sec[r.Offset:], uint32(int32(disp)))
	default:
		return fmt.Errorf("obj: unsupported thread-local GOT relocation type %d (symbol %q)", r.Type, r.Sym)
	}
	return nil
}

// tpOffset turns a thread-local's offset within the TLS block into its offset
// from the thread pointer, which is what a local-exec relocation encodes. The two
// architectures put the block on opposite sides of the pointer:
//
//   - AArch64 (variant I): the pointer addresses a TCB and the block follows it,
//     so offsets are positive, past a two-word TCB rounded up to the block's
//     alignment.
//   - x86-64 (variant II): the pointer addresses the *end* of the block, so
//     offsets are negative.
func tpOffset(machine uint16, symOff, blockSize, align uint64) uint64 {
	switch machine {
	case EM_AARCH64:
		const tcbSize = 16
		return uint64(alignUp(tcbSize, int(align))) + symOff
	case EM_X86_64:
		return symOff - uint64(alignUp(int(blockSize), int(align)))
	}
	return symOff
}

// resolveTLS writes a local-exec relocation: the offset of a thread-local from
// the thread pointer, which is a link-time constant because the executable's
// block sits at a fixed place in every thread.
func resolveTLS(sec []byte, r Reloc, tp int64) error {
	switch r.Type {
	case R_AARCH64_TLSLE_ADD_TPREL_HI12:
		w := binary.LittleEndian.Uint32(sec[r.Offset:])
		w = (w &^ (0xfff << 10)) | (uint32((tp>>12)&0xfff) << 10)
		binary.LittleEndian.PutUint32(sec[r.Offset:], w)
	case R_AARCH64_TLSLE_ADD_TPREL_LO12_NC:
		w := binary.LittleEndian.Uint32(sec[r.Offset:])
		w = (w &^ (0xfff << 10)) | (uint32(tp&0xfff) << 10)
		binary.LittleEndian.PutUint32(sec[r.Offset:], w)
	case R_X86_64_TPOFF32:
		binary.LittleEndian.PutUint32(sec[r.Offset:], uint32(int32(tp)))
	default:
		return fmt.Errorf("obj: unsupported thread-local relocation type %d (symbol %q)", r.Type, r.Sym)
	}
	return nil
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

// irelativeType is the machine's resolver-call relocation: the loader calls the
// function at the addend and stores what it returns.
func irelativeType(machine uint16) uint32 {
	switch machine {
	case EM_AARCH64:
		return R_AARCH64_IRELATIVE
	case EM_X86_64:
		return R_X86_64_IRELATIVE
	}
	return 0
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

// gnuHashTable builds a DT_GNU_HASH table over the defined symbols, which occupy
// dynsym[symoffset:] and must already be ordered by bucket (the format walks a
// bucket as a run of adjacent symbols, so ordering is a hard requirement, not an
// optimization). Undefined symbols are not hashed at all, which is why they must
// come first. Each chain word carries the symbol's hash with bit 0 reused as an
// end-of-bucket marker, and a Bloom filter lets the loader reject a name that is
// certainly absent without touching the buckets.
func gnuHashTable(names []string, symoffset int) []byte {
	n := len(names) - symoffset
	nbuckets := uint32(1)
	if n > 0 {
		nbuckets = uint32(n)
	}
	bloomSize := uint32(1) // must be a power of two
	for int(bloomSize)*8 < n {
		bloomSize *= 2
	}
	const bloomShift = 6

	bloom := make([]uint64, bloomSize)
	buckets := make([]uint32, nbuckets)
	chain := make([]uint32, n)
	for i := 0; i < n; i++ {
		h := gnuHash(names[symoffset+i])
		bloom[(h/64)%bloomSize] |= 1<<(h%64) | 1<<((h>>bloomShift)%64)
		b := h % nbuckets
		if buckets[b] == 0 {
			buckets[b] = uint32(symoffset + i) // first symbol of this bucket
		}
		// Bit 0 marks the last symbol in a bucket; the hash itself is even-masked.
		chain[i] = h &^ 1
		if i+1 == n || gnuHash(names[symoffset+i+1])%nbuckets != b {
			chain[i] |= 1
		}
	}

	w := &elfBuf{}
	w.u32(nbuckets)
	w.u32(uint32(symoffset))
	w.u32(bloomSize)
	w.u32(bloomShift)
	for _, v := range bloom {
		w.u64(v)
	}
	for _, v := range buckets {
		w.u32(v)
	}
	for _, v := range chain {
		w.u32(v)
	}
	return w.b
}

// buildVerneed encodes .gnu.version_r: one Verneed per library, each followed by
// a Vernaux per version required from it. The offsets are relative to the record
// they sit in, and a zero next-offset ends a list.
func buildVerneed(needs []verneed, index map[string]uint16, dynstr *strtab) []byte {
	const entSz = 16 // both Verneed and Vernaux are 16 bytes
	b := &elfBuf{}
	for i, n := range needs {
		vnNext := uint32(0)
		if i != len(needs)-1 {
			vnNext = uint32(entSz * (1 + len(n.vers))) // past this Verneed and its Vernaux run
		}
		b.u16(1)                   // vn_version
		b.u16(uint16(len(n.vers))) // vn_cnt
		b.u32(dynstr.add(n.lib))   // vn_file
		b.u32(entSz)               // vn_aux: the first Vernaux follows immediately
		b.u32(vnNext)
		for j, v := range n.vers {
			vnaNext := uint32(entSz)
			if j == len(n.vers)-1 {
				vnaNext = 0
			}
			b.u32(elfHash(v))            // vna_hash
			b.u16(0)                     // vna_flags
			b.u16(index[n.lib+"\x00"+v]) // vna_other: the .gnu.version index
			b.u32(dynstr.add(v))         // vna_name
			b.u32(vnaNext)
		}
	}
	return b.b
}

// gnuHash is the djb2-derived symbol hash used by DT_GNU_HASH.
func gnuHash(name string) uint32 {
	h := uint32(5381)
	for i := 0; i < len(name); i++ {
		h = h*33 + uint32(name[i])
	}
	return h
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

// pltStubSize is the byte size of one PLT entry (a uniform stride).
func pltStubSize(machine uint16) int {
	switch machine {
	case EM_AARCH64, EM_X86_64:
		return 16
	}
	return 0
}

// plt0Size is the size of the resolver trampoline that heads the PLT. It exists
// only under lazy binding; eager binding needs no resolver.
func plt0Size(machine uint16, lazy bool) int {
	if !lazy {
		return 0
	}
	switch machine {
	case EM_AARCH64:
		return 32
	case EM_X86_64:
		return 16
	}
	return 0
}

// adrpLdrAdd encodes the AArch64 PLT preamble: adrp x16, page(got) ; ldr x17,
// [x16, #lo12(got)] ; add x16, x16, #lo12(got). It leaves x17 holding the GOT
// slot's contents and x16 holding the slot's address -- the resolver identifies
// which import it was called for from that address, which is why the add is here
// even though eager binding never looks at x16.
func adrpLdrAdd(b *elfBuf, atVaddr, gotVaddr uint64) {
	page := ((int64(gotVaddr) &^ 0xfff) - (int64(atVaddr) &^ 0xfff)) >> 12
	lo12 := uint32(gotVaddr & 0xfff)
	b.u32(0x90000010 | uint32(page&3)<<29 | uint32((page>>2)&0x7ffff)<<5) // adrp x16
	b.u32(0xf9400211 | (lo12/8)<<10)                                      // ldr x17, [x16,#lo12]
	b.u32(0x91000210 | lo12<<10)                                          // add x16, x16, #lo12
}

// plt0Stub encodes the lazy-binding resolver trampoline. A PLT entry whose GOT
// slot is still unresolved lands here with x16 (or the stack, on x86-64) naming
// the slot; the trampoline hands that plus the link map in GOT[1] to the
// resolver in GOT[2], which binds the symbol and jumps on to it.
func plt0Stub(machine uint16, plt0Vaddr, gotVaddr uint64) []byte {
	b := &elfBuf{}
	switch machine {
	case EM_AARCH64:
		b.u32(0xa9bf7bf0)                       // stp x16, x30, [sp, #-16]!  -- save the slot address and return address
		adrpLdrAdd(b, plt0Vaddr+4, gotVaddr+16) // point at GOT[2], the resolver
		b.u32(0xd61f0220)                       // br x17
		for i := 0; i < 3; i++ {
			b.u32(0xd503201f) // nop, padding to the 32-byte PLT0
		}
	case EM_X86_64:
		b.bytes([]byte{0xff, 0x35}) // push qword [rip+disp] -- GOT[1], the link map
		b.u32(uint32(int32(int64(gotVaddr+8) - int64(plt0Vaddr+6))))
		b.bytes([]byte{0xff, 0x25}) // jmp qword [rip+disp] -- GOT[2], the resolver
		b.u32(uint32(int32(int64(gotVaddr+16) - int64(plt0Vaddr+12))))
		b.bytes([]byte{0xcc, 0xcc, 0xcc, 0xcc}) // padding to the 16-byte stride
	}
	return b.b
}

// pltStub encodes the PLT entry for import idx: it jumps to whatever address the
// GOT slot at gotVaddr holds. Under eager binding that is always the real target.
// Under lazy binding the slot starts out pointing back into the PLT, so the first
// call reaches the resolver and every later one goes straight through. The GOT is
// addressed PC-relatively, so the entry works wherever the image is loaded.
func pltStub(machine uint16, stubVaddr, gotVaddr, plt0Vaddr uint64, idx int, lazy bool) []byte {
	b := &elfBuf{}
	switch machine {
	case EM_AARCH64:
		adrpLdrAdd(b, stubVaddr, gotVaddr)
		b.u32(0xd61f0220) // br x17
	case EM_X86_64:
		b.bytes([]byte{0xff, 0x25}) // jmp qword [rip+disp] -- the GOT slot
		b.u32(uint32(int32(int64(gotVaddr) - int64(stubVaddr+6))))
		if lazy {
			b.u8(0x68) // push $idx -- tells the resolver which relocation this is
			b.u32(uint32(idx))
			b.u8(0xe9) // jmp plt0
			b.u32(uint32(int32(int64(plt0Vaddr) - int64(stubVaddr+16))))
		} else {
			for i := 0; i < 10; i++ {
				b.u8(0xcc) // int3: eager binding never falls through
			}
		}
	}
	return b.b
}

// pltGotInit is the value a PLT's GOT slot starts with. Lazy binding points it
// back into the PLT so the first call reaches the resolver -- on AArch64 at the
// trampoline itself, on x86-64 at the push that identifies the relocation. The
// loader adds the load bias to whatever is here, so a link-time address is right
// in a PIE too. Eager binding needs no initial value: the loader writes the real
// target before anything runs.
func pltGotInit(machine uint16, lazy bool, plt0Vaddr, stubVaddr uint64) uint64 {
	if !lazy {
		return 0
	}
	if machine == EM_X86_64 {
		return stubVaddr + 6
	}
	return plt0Vaddr
}
