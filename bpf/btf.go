package bpf

import (
	"encoding/binary"
	"fmt"

	"github.com/evanphx/cg12/ir"
)

// BTF (BPF Type Format) type kinds and constants used here.
const (
	btfKindInt       = 1
	btfKindPtr       = 2
	btfKindArray     = 3
	btfKindStruct    = 4
	btfKindFunc      = 12
	btfKindFuncProto = 13
	btfKindVar       = 14
	btfKindDatasec   = 15

	btfVarGlobal = 1
	btfIntSigned = 1 << 24 // the "encoding" byte of an INT's extra word

	btfFuncStatic = 0 // FUNC linkage: a subprogram
	btfFuncGlobal = 1 // FUNC linkage: an entry program
)

// btfBuilder assembles a .BTF blob: a header, a type section, and a string
// section. Types are referenced by 1-based id in the order they are added.
type btfBuilder struct {
	types  []byte
	strs   []byte
	strOff map[string]uint32
	n      uint32 // id of the last type added
}

func newBTFBuilder() *btfBuilder {
	return &btfBuilder{strs: []byte{0}, strOff: map[string]uint32{"": 0}}
}

// str interns a string and returns its offset in the string section.
func (b *btfBuilder) str(s string) uint32 {
	if o, ok := b.strOff[s]; ok {
		return o
	}
	o := uint32(len(b.strs))
	b.strs = append(b.strs, s...)
	b.strs = append(b.strs, 0)
	b.strOff[s] = o
	return o
}

// emit appends one type (a 12-byte header plus any extra words) and returns its id.
func (b *btfBuilder) emit(nameOff, info, sizeType uint32, extra ...uint32) uint32 {
	w := func(v uint32) {
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], v)
		b.types = append(b.types, buf[:]...)
	}
	w(nameOff)
	w(info)
	w(sizeType)
	for _, e := range extra {
		w(e)
	}
	b.n++
	return b.n
}

func (b *btfBuilder) intType(name string, size, encoding uint32) uint32 {
	return b.emit(b.str(name), btfKindInt<<24, size, (encoding&0xff000000)|(size*8))
}

func (b *btfBuilder) ptr(to uint32) uint32 { return b.emit(0, btfKindPtr<<24, to) }

func (b *btfBuilder) array(elem, index, nelems uint32) uint32 {
	return b.emit(0, btfKindArray<<24, 0, elem, index, nelems)
}

// structType emits an anonymous struct; members are (name-offset, type, bit-offset).
func (b *btfBuilder) structType(size uint32, members [][3]uint32) uint32 {
	extra := make([]uint32, 0, len(members)*3)
	for _, m := range members {
		extra = append(extra, m[0], m[1], m[2])
	}
	return b.emit(0, uint32(btfKindStruct<<24)|uint32(len(members)), size, extra...)
}

func (b *btfBuilder) varType(name string, typ, linkage uint32) uint32 {
	return b.emit(b.str(name), btfKindVar<<24, typ, linkage)
}

// funcProto emits a FUNC_PROTO: a return type (0 for void) and its parameters,
// each a (name, type id) pair. Global functions require named parameters.
func (b *btfBuilder) funcProto(ret uint32, params []uint32, names []string) uint32 {
	extra := make([]uint32, 0, len(params)*2)
	for i, p := range params {
		var nameOff uint32
		if i < len(names) {
			nameOff = b.str(names[i])
		}
		extra = append(extra, nameOff, p)
	}
	return b.emit(0, uint32(btfKindFuncProto<<24)|uint32(len(params)), ret, extra...)
}

// funcType emits a FUNC named name, of the given prototype, with a linkage
// (global for an entry program, static for a subprogram).
func (b *btfBuilder) funcType(name string, proto, linkage uint32) uint32 {
	return b.emit(b.str(name), uint32(btfKindFunc<<24)|linkage, proto)
}

// datasec emits a DATASEC; entries are (var-type-id, byte-offset, byte-size).
func (b *btfBuilder) datasec(name string, entries [][3]uint32) uint32 {
	extra := make([]uint32, 0, len(entries)*3)
	for _, e := range entries {
		extra = append(extra, e[0], e[1], e[2])
	}
	return b.emit(b.str(name), uint32(btfKindDatasec<<24)|uint32(len(entries)), 0, extra...)
}

// blob renders the header + type section + string section.
func (b *btfBuilder) blob() []byte {
	var hdr [24]byte
	binary.LittleEndian.PutUint16(hdr[0:], 0xeb9f) // magic
	hdr[2] = 1                                     // version
	binary.LittleEndian.PutUint32(hdr[4:], 24)     // hdr_len
	binary.LittleEndian.PutUint32(hdr[8:], 0)      // type_off
	binary.LittleEndian.PutUint32(hdr[12:], uint32(len(b.types)))
	binary.LittleEndian.PutUint32(hdr[16:], uint32(len(b.types))) // str_off
	binary.LittleEndian.PutUint32(hdr[20:], uint32(len(b.strs)))
	out := append([]byte{}, hdr[:]...)
	out = append(out, b.types...)
	return append(out, b.strs...)
}

// buildBTF produces the .BTF blob describing the object's BTF-defined maps, its
// data sections, and a FUNC type for every compiled function — the type
// information modern libbpf requires. It returns the built types (not yet
// rendered) and each function's FUNC type id, so the caller can add .BTF.ext
// strings to the same string section before rendering with the builder's blob.
func buildBTF(obj *Object) (*btfBuilder, map[string]uint32) {
	b := newBTFBuilder()
	intT := b.intType("int", 4, btfIntSigned)
	idxT := b.intType("__ARRAY_SIZE_TYPE__", 4, 0)
	byteT := b.intType("__u8", 1, 0)

	// One byte array of size n, for sizing a rodata var.
	byteArr := func(n uint32) uint32 {
		if n == 0 {
			return b.ptr(0) // void*: a zero-sized member
		}
		return b.array(byteT, idxT, n)
	}

	// scalar returns a type of exactly n bytes for a map key or value: an integer
	// for a standard width (the kernel requires an int key for some map types),
	// otherwise a byte array.
	intCache := map[uint32]uint32{4: intT}
	scalar := func(n uint32) uint32 {
		switch n {
		case 1, 2, 4, 8:
			if id, ok := intCache[n]; ok {
				return id
			}
			id := b.intType(fmt.Sprintf("__u%d", n*8), n, 0)
			intCache[n] = id
			return id
		default:
			return byteArr(n)
		}
	}

	var mapVars [][3]uint32
	off := uint32(0)
	for _, m := range obj.Maps {
		if isDataSection(m.Name) {
			continue // .rodata / .data are DATASECs, not BTF-defined maps
		}
		st := b.structType(32, [][3]uint32{
			{b.str("type"), b.ptr(b.array(intT, idxT, m.Type)), 0},
			{b.str("max_entries"), b.ptr(b.array(intT, idxT, m.MaxEntries)), 64},
			{b.str("key"), b.ptr(scalar(m.KeySize)), 128},
			{b.str("value"), b.ptr(scalar(m.ValueSize)), 192},
		})
		mapVars = append(mapVars, [3]uint32{b.varType(m.Name, st, btfVarGlobal), off, 32})
		off += 32
	}
	if len(mapVars) > 0 {
		b.datasec(".maps", mapVars)
	}

	// One DATASEC per data section, holding a VAR for each global it contains.
	for _, sec := range []string{rodataName, dataName} {
		var vars [][3]uint32
		for _, rv := range obj.DataVars {
			if rv.Section != sec {
				continue
			}
			v := b.varType(rv.Name, byteArr(rv.Size), btfVarGlobal)
			vars = append(vars, [3]uint32{v, rv.Off, rv.Size})
		}
		if len(vars) > 0 {
			b.datasec(sec, vars)
		}
	}

	// A FUNC (over a FUNC_PROTO) per compiled function, so a .BTF.ext func_info
	// record can name it in the xlated dump. Parameter and return types are
	// modelled by width — enough for a well-formed prototype.
	clsType := func(c ir.Cls) uint32 { return scalar(uint32(c.Size())) }
	funcTypes := map[string]uint32{}
	for _, f := range obj.Funcs {
		var params []uint32
		for _, p := range f.Sig.Params {
			params = append(params, clsType(p))
		}
		var ret uint32
		if f.Sig.HasRet {
			ret = clsType(f.Sig.Ret)
		}
		proto := b.funcProto(ret, params, f.Sig.ParamNames)
		linkage := uint32(btfFuncStatic)
		if f.Section != "" {
			linkage = btfFuncGlobal
		}
		funcTypes[f.Name] = b.funcType(f.Name, proto, linkage)
	}
	return b, funcTypes
}

// isDataSection reports whether name is one of the synthetic data-section maps.
func isDataSection(name string) bool { return name == rodataName || name == dataName }

// btfExtFunc describes one function's placement for .BTF.ext: the ELF section it
// lives in, its name (a key into the FUNC-type map), its instruction offset
// within that section, and the source position of each of its instructions.
type btfExtFunc struct {
	section string
	name    string
	insnOff uint32
	poss    []ir.SrcPos
}

// putU32 appends a little-endian uint32.
func putU32(dst *[]byte, v uint32) {
	var t [4]byte
	binary.LittleEndian.PutUint32(t[:], v)
	*dst = append(*dst, t[:]...)
}

// buildBTFExt renders a .BTF.ext section: func_info records that name each
// function in the xlated dump, and line_info records that map instructions back
// to source positions. Section and file-name strings are interned into b so they
// share .BTF's string section — the caller renders .BTF with b.blob() afterward.
// Records are grouped by ELF section and, within a section, ordered by
// instruction offset (the order functions appear in funcs), as the kernel
// requires. It returns nil when there is nothing to describe.
func buildBTFExt(b *btfBuilder, funcTypes map[string]uint32, files []string, funcs []btfExtFunc) []byte {
	if len(funcs) == 0 {
		return nil
	}
	var order []string
	bySec := map[string][]btfExtFunc{}
	for _, f := range funcs {
		if _, ok := bySec[f.section]; !ok {
			order = append(order, f.section)
		}
		bySec[f.section] = append(bySec[f.section], f)
	}

	// func_info: rec_size, then a (sec_name, count, records) group per section.
	funcSection := []byte{}
	putU32(&funcSection, 8) // sizeof(bpf_func_info)
	for _, sec := range order {
		fns := bySec[sec]
		putU32(&funcSection, b.str(sec))
		putU32(&funcSection, uint32(len(fns)))
		for _, f := range fns {
			putU32(&funcSection, f.insnOff*8) // .BTF.ext insn offsets are in bytes
			putU32(&funcSection, funcTypes[f.name])
		}
	}

	// line_info: a record whenever a function's source line changes, with a record
	// forced at each function's first instruction (the kernel requires the first
	// instruction of a loaded program to carry line info). line_off is left empty
	// (0): positions carry file, line, and column but not the source text.
	fileOff := func(idx uint32) uint32 {
		if idx == 0 || int(idx) > len(files) {
			return 0
		}
		return b.str(files[idx-1])
	}
	lineSection := []byte{}
	putU32(&lineSection, 16) // sizeof(bpf_line_info)
	for _, sec := range order {
		var recs []byte
		nrec := 0
		for _, f := range bySec[sec] {
			var firstValid ir.SrcPos
			for _, p := range f.poss {
				if p.Valid() {
					firstValid = p
					break
				}
			}
			var prev ir.SrcPos
			for j, p := range f.poss {
				use := p
				if j == 0 && !use.Valid() {
					use = firstValid // attribute the prologue to the function's first line
				}
				if !use.Valid() || use == prev {
					continue
				}
				prev = use
				putU32(&recs, (f.insnOff+uint32(j))*8) // .BTF.ext insn offsets are in bytes
				putU32(&recs, fileOff(use.File))
				putU32(&recs, 0)
				putU32(&recs, use.Line<<10|(use.Col&0x3ff))
				nrec++
			}
		}
		if nrec == 0 {
			continue
		}
		putU32(&lineSection, b.str(sec))
		putU32(&lineSection, uint32(nrec))
		lineSection = append(lineSection, recs...)
	}

	const hdrLen = 32
	out := make([]byte, hdrLen)
	binary.LittleEndian.PutUint16(out[0:], 0xeb9f) // magic
	out[2] = 1                                     // version
	// flags (out[3]) = 0
	binary.LittleEndian.PutUint32(out[4:], hdrLen)
	binary.LittleEndian.PutUint32(out[8:], 0)                         // func_info_off
	binary.LittleEndian.PutUint32(out[12:], uint32(len(funcSection))) // func_info_len
	binary.LittleEndian.PutUint32(out[16:], uint32(len(funcSection))) // line_info_off
	binary.LittleEndian.PutUint32(out[20:], uint32(len(lineSection))) // line_info_len
	binary.LittleEndian.PutUint32(out[24:], 0)                        // core_relo_off
	binary.LittleEndian.PutUint32(out[28:], 0)                        // core_relo_len
	out = append(out, funcSection...)
	out = append(out, lineSection...)
	return out
}
