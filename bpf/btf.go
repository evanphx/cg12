package bpf

import (
	"encoding/binary"
	"fmt"
)

// BTF (BPF Type Format) type kinds and constants used here.
const (
	btfKindInt     = 1
	btfKindPtr     = 2
	btfKindArray   = 3
	btfKindStruct  = 4
	btfKindVar     = 14
	btfKindDatasec = 15

	btfVarGlobal = 1
	btfIntSigned = 1 << 24 // the "encoding" byte of an INT's extra word
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

// buildBTF produces the .BTF blob describing the object's BTF-defined maps and
// its .rodata section — the type information modern libbpf requires to create
// the maps.
func buildBTF(obj *Object) []byte {
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
	return b.blob()
}

// isDataSection reports whether name is one of the synthetic data-section maps.
func isDataSection(name string) bool { return name == rodataName || name == dataName }
